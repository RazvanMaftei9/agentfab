package identity

import "context"

type CertificateProvider interface {
	Name() string
	IssueCertificate(ctx context.Context, req IssueRequest) (IssuedCertificate, error)
	RenewCertificate(ctx context.Context, current IssuedCertificate, req IssueRequest) (IssuedCertificate, error)
	TrustBundle(ctx context.Context) (TrustBundle, error)
}

type Attestor interface {
	Name() string
	AttestNode(ctx context.Context, request NodeAttestation) (AttestedNode, error)
}

type EnrollmentAuthority interface {
	Name() string
	IssueNodeToken(ctx context.Context, request NodeTokenRequest) (NodeEnrollmentToken, error)
}
