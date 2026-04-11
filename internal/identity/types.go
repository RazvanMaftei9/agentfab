package identity

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"time"
)

type SubjectKind string

const (
	SubjectKindControlPlane  SubjectKind = "control_plane"
	SubjectKindConductor     SubjectKind = "conductor"
	SubjectKindNode          SubjectKind = "node"
	SubjectKindAgentProfile  SubjectKind = "agent_profile"
	SubjectKindAgentInstance SubjectKind = "agent_instance"
)

type Subject struct {
	TrustDomain string
	Fabric      string
	Kind        SubjectKind
	Name        string
	NodeID      string
	Profile     string
	InstanceID  string
}

func (s Subject) Validate() error {
	if s.TrustDomain == "" {
		return fmt.Errorf("trust domain is required")
	}
	if s.Fabric == "" {
		return fmt.Errorf("fabric is required")
	}
	if s.Kind == "" {
		return fmt.Errorf("subject kind is required")
	}
	if s.Name == "" {
		return fmt.Errorf("subject name is required")
	}

	switch s.Kind {
	case SubjectKindControlPlane:
		return nil
	case SubjectKindConductor:
		return nil
	case SubjectKindNode:
		if s.NodeID == "" {
			return fmt.Errorf("node ID is required")
		}
	case SubjectKindAgentProfile:
		if s.Profile == "" {
			return fmt.Errorf("profile is required")
		}
	case SubjectKindAgentInstance:
		if s.NodeID == "" {
			return fmt.Errorf("node ID is required")
		}
		if s.Profile == "" {
			return fmt.Errorf("profile is required")
		}
		if s.InstanceID == "" {
			return fmt.Errorf("instance ID is required")
		}
	default:
		return fmt.Errorf("unknown subject kind %q", s.Kind)
	}

	return nil
}

func (s Subject) URI() string {
	base := path.Join("/fabric", NormalizeID(s.Fabric))
	switch s.Kind {
	case SubjectKindControlPlane:
		return "spiffe://" + s.TrustDomain + path.Join(base, "control-plane", NormalizeID(s.Name))
	case SubjectKindConductor:
		return "spiffe://" + s.TrustDomain + path.Join(base, "conductor", NormalizeID(s.Name))
	case SubjectKindNode:
		return "spiffe://" + s.TrustDomain + path.Join(base, "node", NormalizeID(s.NodeID))
	case SubjectKindAgentProfile:
		return "spiffe://" + s.TrustDomain + path.Join(base, "agent-profile", NormalizeID(s.Profile))
	case SubjectKindAgentInstance:
		return "spiffe://" + s.TrustDomain + path.Join(
			base,
			"node", NormalizeID(s.NodeID),
			"agent", NormalizeID(s.Profile),
			"instance", NormalizeID(s.InstanceID),
		)
	default:
		return "spiffe://" + s.TrustDomain + path.Join(base, "unknown", NormalizeID(s.Name))
	}
}

func ParseSubjectURI(raw string) (Subject, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Subject{}, fmt.Errorf("parse subject URI: %w", err)
	}
	if parsed.Scheme != "spiffe" {
		return Subject{}, fmt.Errorf("unsupported subject URI scheme %q", parsed.Scheme)
	}

	segments := splitPathSegments(parsed.Path)
	if len(segments) < 3 || segments[0] != "fabric" {
		return Subject{}, fmt.Errorf("invalid subject URI path %q", parsed.Path)
	}

	subject := Subject{
		TrustDomain: parsed.Host,
		Fabric:      segments[1],
	}

	switch {
	case len(segments) == 4 && segments[2] == "control-plane":
		subject.Kind = SubjectKindControlPlane
		subject.Name = segments[3]
	case len(segments) == 4 && segments[2] == "conductor":
		subject.Kind = SubjectKindConductor
		subject.Name = segments[3]
	case len(segments) == 4 && segments[2] == "node":
		subject.Kind = SubjectKindNode
		subject.Name = segments[3]
		subject.NodeID = segments[3]
	case len(segments) == 4 && segments[2] == "agent-profile":
		subject.Kind = SubjectKindAgentProfile
		subject.Name = segments[3]
		subject.Profile = segments[3]
	case len(segments) == 8 && segments[2] == "node" && segments[4] == "agent" && segments[6] == "instance":
		subject.Kind = SubjectKindAgentInstance
		subject.NodeID = segments[3]
		subject.Profile = segments[5]
		subject.InstanceID = segments[7]
		subject.Name = segments[5]
	default:
		return Subject{}, fmt.Errorf("unsupported subject URI path %q", parsed.Path)
	}

	if err := subject.Validate(); err != nil {
		return Subject{}, err
	}
	return subject, nil
}

func SubjectFromCertificate(cert *x509.Certificate) (Subject, error) {
	if cert == nil {
		return Subject{}, fmt.Errorf("certificate is required")
	}
	if len(cert.URIs) == 0 {
		return Subject{}, fmt.Errorf("certificate missing identity URI")
	}
	return ParseSubjectURI(cert.URIs[0].String())
}

func NormalizeID(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "-")
	value = strings.ReplaceAll(value, "/", "-")
	return value
}

func splitPathSegments(value string) []string {
	value = strings.Trim(value, "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

type IssueRequest struct {
	Subject      Subject
	Principal    string
	DNSNames     []string
	IPAddresses  []net.IP
	RequestedTTL time.Duration
}

type TrustBundle struct {
	TrustDomain string
	RootCAPEM   []byte
	RootCAs     *x509.CertPool
}

type IssuedCertificate struct {
	Subject     Subject
	Principal   string
	IdentityURI string
	Certificate tls.Certificate
	ServerTLS   *tls.Config
	ClientTLS   *tls.Config
	TrustBundle TrustBundle
	ExpiresAt   time.Time
}

type NodeAttestation struct {
	Type         string
	Claims       map[string]string
	Measurements map[string]string
	Token        string
}

type AttestedNode struct {
	NodeID       string
	TrustDomain  string
	Claims       map[string]string
	Measurements map[string]string
	AttestedAt   time.Time
	ExpiresAt    time.Time
}

type NodeTokenRequest struct {
	Fabric               string
	NodeID               string
	Description          string
	ExpiresAt            time.Time
	Reusable             bool
	ExpectedMeasurements map[string]string
}

type NodeEnrollmentToken struct {
	ID                   string
	Value                string
	Fabric               string
	NodeID               string
	Description          string
	ExpiresAt            time.Time
	Reusable             bool
	CreatedAt            time.Time
	ExpectedMeasurements map[string]string
}
