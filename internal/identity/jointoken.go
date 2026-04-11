package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const (
	NodeJoinTokenAttestationType = "join_token"
	joinTokenStatePath           = "shared/identity/node-enrollment-tokens.json"
)

type LocalDevJoinTokenAuthority struct {
	dataDir     string
	trustDomain string
}

type joinTokenState struct {
	Tokens []joinTokenRecord `json:"tokens"`
}

type joinTokenRecord struct {
	ID                   string            `json:"id"`
	TokenHash            string            `json:"token_hash"`
	Fabric               string            `json:"fabric"`
	NodeID               string            `json:"node_id"`
	Description          string            `json:"description"`
	ExpectedMeasurements map[string]string `json:"expected_measurements,omitempty"`
	ExpiresAt            time.Time         `json:"expires_at"`
	Reusable             bool              `json:"reusable"`
	CreatedAt            time.Time         `json:"created_at"`
	LastUsedAt           time.Time         `json:"last_used_at"`
	LastUsedNode         string            `json:"last_used_node"`
	UseCount             int               `json:"use_count"`
}

func NewLocalDevJoinTokenAuthority(dataDir, trustDomain string) *LocalDevJoinTokenAuthority {
	return &LocalDevJoinTokenAuthority{
		dataDir:     dataDir,
		trustDomain: trustDomain,
	}
}

func (a *LocalDevJoinTokenAuthority) Name() string {
	return "local-dev-join-token"
}

func (a *LocalDevJoinTokenAuthority) IssueNodeToken(_ context.Context, request NodeTokenRequest) (NodeEnrollmentToken, error) {
	if request.ExpiresAt.IsZero() {
		request.ExpiresAt = time.Now().Add(24 * time.Hour)
	}
	if request.ExpiresAt.Before(time.Now()) {
		return NodeEnrollmentToken{}, fmt.Errorf("token expiration must be in the future")
	}

	tokenValue, err := randomToken()
	if err != nil {
		return NodeEnrollmentToken{}, err
	}
	record := joinTokenRecord{
		ID:                   tokenID(tokenValue),
		TokenHash:            tokenHash(tokenValue),
		Fabric:               request.Fabric,
		NodeID:               request.NodeID,
		Description:          request.Description,
		ExpectedMeasurements: cloneStringMap(request.ExpectedMeasurements),
		ExpiresAt:            request.ExpiresAt.UTC(),
		Reusable:             request.Reusable,
		CreatedAt:            time.Now().UTC(),
	}

	if err := a.upsertRecord(record); err != nil {
		return NodeEnrollmentToken{}, err
	}

	return NodeEnrollmentToken{
		ID:                   record.ID,
		Value:                tokenValue,
		Fabric:               record.Fabric,
		NodeID:               record.NodeID,
		Description:          record.Description,
		ExpiresAt:            record.ExpiresAt,
		Reusable:             record.Reusable,
		CreatedAt:            record.CreatedAt,
		ExpectedMeasurements: cloneStringMap(record.ExpectedMeasurements),
	}, nil
}

func (a *LocalDevJoinTokenAuthority) AttestNode(_ context.Context, request NodeAttestation) (AttestedNode, error) {
	if request.Type != NodeJoinTokenAttestationType {
		return AttestedNode{}, fmt.Errorf("unsupported attestation type %q", request.Type)
	}
	if request.Token == "" {
		return AttestedNode{}, fmt.Errorf("node enrollment token is required")
	}

	state, lockFile, err := a.lockedState()
	if err != nil {
		return AttestedNode{}, err
	}
	defer unlockJoinTokenState(lockFile)

	now := time.Now().UTC()
	recordIndex := -1
	expectedHash := tokenHash(request.Token)
	for i, record := range state.Tokens {
		if subtle.ConstantTimeCompare([]byte(record.TokenHash), []byte(expectedHash)) == 1 {
			recordIndex = i
			break
		}
	}
	if recordIndex < 0 {
		return AttestedNode{}, fmt.Errorf("node enrollment token is invalid")
	}

	record := state.Tokens[recordIndex]
	if now.After(record.ExpiresAt) {
		return AttestedNode{}, fmt.Errorf("node enrollment token has expired")
	}
	if !record.Reusable && record.UseCount > 0 {
		return AttestedNode{}, fmt.Errorf("node enrollment token has already been used")
	}

	nodeID := request.Claims["node_id"]
	if record.NodeID != "" {
		if nodeID != "" && nodeID != record.NodeID {
			return AttestedNode{}, fmt.Errorf("node enrollment token is bound to node %q", record.NodeID)
		}
		nodeID = record.NodeID
	}
	if nodeID == "" {
		return AttestedNode{}, fmt.Errorf("node ID is required for attestation")
	}

	if record.Fabric != "" {
		fabric := request.Claims["fabric"]
		if fabric == "" {
			return AttestedNode{}, fmt.Errorf("fabric claim is required for attestation")
		}
		if fabric != record.Fabric {
			return AttestedNode{}, fmt.Errorf("node enrollment token is bound to fabric %q", record.Fabric)
		}
	}
	for key, expectedValue := range record.ExpectedMeasurements {
		actualValue := request.Measurements[key]
		if actualValue == "" {
			return AttestedNode{}, fmt.Errorf("node attestation measurement %q is required", key)
		}
		if actualValue != expectedValue {
			return AttestedNode{}, fmt.Errorf("node attestation measurement %q does not match expected value", key)
		}
	}

	record.LastUsedAt = now
	record.LastUsedNode = nodeID
	record.UseCount++
	state.Tokens[recordIndex] = record
	if err := saveJoinTokenState(a.stateFilePath(), state); err != nil {
		return AttestedNode{}, err
	}

	claims := map[string]string{
		"attestor": a.Name(),
		"token_id": record.ID,
	}
	if record.Fabric != "" {
		claims["fabric"] = record.Fabric
	}
	if record.NodeID != "" {
		claims["bound_node_id"] = record.NodeID
	}

	return AttestedNode{
		NodeID:       nodeID,
		TrustDomain:  a.trustDomain,
		Claims:       claims,
		Measurements: cloneStringMap(request.Measurements),
		AttestedAt:   now,
		ExpiresAt:    record.ExpiresAt,
	}, nil
}

func (a *LocalDevJoinTokenAuthority) upsertRecord(record joinTokenRecord) error {
	state, lockFile, err := a.lockedState()
	if err != nil {
		return err
	}
	defer unlockJoinTokenState(lockFile)

	replaced := false
	for i, existing := range state.Tokens {
		if existing.ID == record.ID {
			state.Tokens[i] = record
			replaced = true
			break
		}
	}
	if !replaced {
		state.Tokens = append(state.Tokens, record)
	}
	sort.Slice(state.Tokens, func(i, j int) bool {
		if state.Tokens[i].CreatedAt.Equal(state.Tokens[j].CreatedAt) {
			return state.Tokens[i].ID < state.Tokens[j].ID
		}
		return state.Tokens[i].CreatedAt.Before(state.Tokens[j].CreatedAt)
	})
	return saveJoinTokenState(a.stateFilePath(), state)
}

func (a *LocalDevJoinTokenAuthority) lockedState() (*joinTokenState, *os.File, error) {
	statePath := a.stateFilePath()
	lockFile, err := lockJoinTokenState(statePath)
	if err != nil {
		return nil, nil, err
	}
	state, err := loadJoinTokenState(statePath)
	if err != nil {
		unlockJoinTokenState(lockFile)
		return nil, nil, err
	}
	return state, lockFile, nil
}

func (a *LocalDevJoinTokenAuthority) stateFilePath() string {
	return filepath.Join(a.dataDir, joinTokenStatePath)
}

func loadJoinTokenState(path string) (*joinTokenState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &joinTokenState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read join token state: %w", err)
	}

	var state joinTokenState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse join token state: %w", err)
	}
	return &state, nil
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func saveJoinTokenState(path string, state *joinTokenState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create join token dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal join token state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".node-enrollment-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create join token temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write join token temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close join token temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("persist join token state: %w", err)
	}
	return nil
}

func lockJoinTokenState(path string) (*os.File, error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("create join token lock dir: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open join token lock file: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire join token lock: %w", err)
	}
	return file, nil
}

func unlockJoinTokenState(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func tokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func tokenID(value string) string {
	hash := tokenHash(value)
	if len(hash) < 16 {
		return hash
	}
	return hash[:16]
}
