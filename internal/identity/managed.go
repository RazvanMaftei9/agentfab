package identity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/razvanmaftei/agentfab/internal/mtls"
)

const (
	certificateRetryDelay = time.Minute
)

type ManagedCertificate struct {
	provider CertificateProvider
	request  IssueRequest

	mu        sync.RWMutex
	current   IssuedCertificate
	serverTLS *tls.Config
	clientTLS *tls.Config
	cancel    context.CancelFunc
	done      chan struct{}
	updates   chan struct{}
}

func NewManagedCertificate(ctx context.Context, provider CertificateProvider, request IssueRequest) (*ManagedCertificate, error) {
	if provider == nil {
		return nil, fmt.Errorf("certificate provider is required")
	}

	issued, err := provider.IssueCertificate(ctx, request)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	m := &ManagedCertificate{
		provider: provider,
		request:  request,
		current:  issued,
		cancel:   cancel,
		done:     make(chan struct{}),
		updates:  make(chan struct{}, 1),
	}
	m.serverTLS = mtls.DynamicServerTLSConfig(m)
	m.clientTLS = mtls.DynamicClientTLSConfig(m)

	go m.run(runCtx)

	return m, nil
}

func (m *ManagedCertificate) ServerTLS() *tls.Config {
	return m.serverTLS
}

func (m *ManagedCertificate) ClientTLS() *tls.Config {
	return m.clientTLS
}

func (m *ManagedCertificate) Current() IssuedCertificate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *ManagedCertificate) CurrentTLSMaterial() (tls.Certificate, *x509.CertPool, error) {
	issued := m.Current()
	if len(issued.Certificate.Certificate) == 0 {
		return tls.Certificate{}, nil, fmt.Errorf("managed certificate is not initialized")
	}

	bundle, err := m.provider.TrustBundle(context.Background())
	if err == nil && bundle.RootCAs != nil {
		return issued.Certificate, bundle.RootCAs, nil
	}
	if issued.TrustBundle.RootCAs == nil {
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		return tls.Certificate{}, nil, fmt.Errorf("managed certificate trust bundle is missing root CAs")
	}
	return issued.Certificate, issued.TrustBundle.RootCAs, nil
}

func (m *ManagedCertificate) Close() {
	if m == nil {
		return
	}
	m.cancel()
	<-m.done
}

func (m *ManagedCertificate) Updates() <-chan struct{} {
	return m.updates
}

func (m *ManagedCertificate) run(ctx context.Context) {
	defer close(m.done)

	for {
		wait := renewalDelay(time.Now(), m.Current().ExpiresAt)
		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if err := m.renew(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("certificate renewal failed", "subject", m.request.Subject.URI(), "provider", m.provider.Name(), "error", err)

			retryTimer := time.NewTimer(certificateRetryDelay)
			select {
			case <-ctx.Done():
				retryTimer.Stop()
				return
			case <-retryTimer.C:
				continue
			}
		}
	}
}

func (m *ManagedCertificate) renew(ctx context.Context) error {
	current := m.Current()
	issued, err := m.provider.RenewCertificate(ctx, current, m.request)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.current = issued
	m.mu.Unlock()
	select {
	case m.updates <- struct{}{}:
	default:
	}
	return nil
}

func renewalDelay(now, expiresAt time.Time) time.Duration {
	ttl := expiresAt.Sub(now)
	if ttl <= 0 {
		return 0
	}

	margin := refreshMargin(ttl)
	if ttl <= margin {
		return 0
	}
	return ttl - margin
}

func refreshMargin(ttl time.Duration) time.Duration {
	switch {
	case ttl <= 10*time.Minute:
		return ttl / 2
	case ttl <= time.Hour:
		return 5 * time.Minute
	case ttl <= 24*time.Hour:
		return time.Hour
	case ttl <= 7*24*time.Hour:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}
