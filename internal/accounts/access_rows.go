//go:build !windows

package accounts

import (
	"encoding/binary"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/account"
)

// The rows map every field of access's records by hand (account's rule 20):
// the records keep their hashes out of JSON, so a row holds each hash's text.
// A row's key is not repeated in it.

type userRow struct {
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Username       string
	Email          string
	DisplayName    string
	PasswordHash   string
	Role           access.Role
	OIDCIssuer     string
	OIDCSubject    string
	WebAuthnHandle []byte
	Enabled        bool
}

func toUserRow(u *access.User) userRow {
	return userRow{
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt, Username: u.Username, Email: u.Email,
		DisplayName: u.DisplayName, PasswordHash: u.PasswordHash.Reveal(), Role: u.Role,
		OIDCIssuer: u.OIDC.Issuer, OIDCSubject: u.OIDC.Subject, WebAuthnHandle: u.WebAuthnHandle, Enabled: u.Enabled,
	}
}

func (v *userRow) user(id int64) *access.User {
	return &access.User{
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, Username: v.Username, Email: v.Email,
		DisplayName: v.DisplayName, PasswordHash: access.NewHash(v.PasswordHash), Role: v.Role,
		OIDC: access.OIDCIdentity{Issuer: v.OIDCIssuer, Subject: v.OIDCSubject}, WebAuthnHandle: v.WebAuthnHandle,
		ID: id, Enabled: v.Enabled,
	}
}

// oidcKey is the users_by_oidc key: the issuer's length, the issuer, the
// subject; nil for the zero identity, which links nothing.
func oidcKey(issuer, subject string) []byte {
	if issuer == "" && subject == "" {
		return nil
	}
	b := binary.AppendUvarint(nil, uint64(len(issuer)))
	return append(append(b, issuer...), subject...)
}

type sessionRow struct {
	CreatedAt    time.Time
	LastActivity time.Time
	AuthTime     time.Time
	TokenHash    string
	AuthMethod   access.Method
	Kind         access.SessionKind
	UserAgent    string
	Label        string
	LastIP       string
	UserID       int64
	IssuedByKey  int64
	IdleCap      time.Duration
	AbsoluteCap  time.Duration
}

func toSessionRow(s *access.Session) sessionRow {
	return sessionRow{
		CreatedAt: s.CreatedAt, LastActivity: s.LastActivity, AuthTime: s.AuthTime, TokenHash: s.TokenHash.Reveal(),
		AuthMethod: s.AuthMethod, Kind: s.Kind, UserAgent: s.UserAgent, Label: s.Label, LastIP: s.LastIP,
		UserID: s.UserID, IssuedByKey: s.IssuedByKey, IdleCap: s.IdleCap, AbsoluteCap: s.AbsoluteCap,
	}
}

func (v *sessionRow) session(id []byte) *access.Session {
	return &access.Session{
		CreatedAt: v.CreatedAt, LastActivity: v.LastActivity, AuthTime: v.AuthTime, ID: string(id),
		TokenHash: access.NewHash(v.TokenHash), AuthMethod: v.AuthMethod, Kind: v.Kind, UserAgent: v.UserAgent,
		Label: v.Label, LastIP: v.LastIP, UserID: v.UserID, IssuedByKey: v.IssuedByKey,
		IdleCap: v.IdleCap, AbsoluteCap: v.AbsoluteCap,
	}
}

type passkeyRow struct {
	CreatedAt         time.Time
	LastUsedAt        time.Time
	Discoverable      *bool
	AttestationType   string
	AttestationFormat string
	RPID              string
	Transport         string
	Name              string
	Attachment        access.AuthenticatorAttachment
	CredentialID      []byte
	PublicKey         []byte
	AAGUID            []byte
	RawAttestation    []byte
	UserID            int64
	SignCount         uint32
	RawFlags          uint8
	BackupEligible    bool
	BackupState       bool
	UserPresent       bool
	UserVerified      bool
	CloneWarning      bool
}

func toPasskeyRow(c *access.PasskeyCredential) passkeyRow {
	return passkeyRow{
		CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt, Discoverable: c.Discoverable,
		AttestationType: c.AttestationType, AttestationFormat: c.AttestationFormat, RPID: c.RPID,
		Transport: c.Transport, Name: c.Name, Attachment: c.Attachment, CredentialID: c.CredentialID,
		PublicKey: c.PublicKey, AAGUID: c.AAGUID, RawAttestation: c.RawAttestation, UserID: c.UserID,
		SignCount: c.SignCount, RawFlags: c.RawFlags, BackupEligible: c.BackupEligible, BackupState: c.BackupState,
		UserPresent: c.UserPresent, UserVerified: c.UserVerified, CloneWarning: c.CloneWarning,
	}
}

func (v *passkeyRow) passkey(id int64) access.PasskeyCredential {
	return access.PasskeyCredential{
		CreatedAt: v.CreatedAt, LastUsedAt: v.LastUsedAt, Discoverable: v.Discoverable,
		AttestationType: v.AttestationType, AttestationFormat: v.AttestationFormat, RPID: v.RPID,
		Transport: v.Transport, Name: v.Name, Attachment: v.Attachment, CredentialID: v.CredentialID,
		PublicKey: v.PublicKey, AAGUID: v.AAGUID, RawAttestation: v.RawAttestation, ID: id, UserID: v.UserID,
		SignCount: v.SignCount, RawFlags: v.RawFlags, BackupEligible: v.BackupEligible, BackupState: v.BackupState,
		UserPresent: v.UserPresent, UserVerified: v.UserVerified, CloneWarning: v.CloneWarning,
	}
}

type keyRow struct {
	CreatedAt     time.Time
	ExpiresAt     time.Time
	LastUsedAt    time.Time
	PreviousUntil time.Time
	KeyHash       string
	PreviousHash  string
	KeyPrefix     string
	KeySuffix     string
	Label         string
	Scopes        []string
	LineageIDs    []int64
	UserID        int64
}

func toKeyRow(k *access.Key) keyRow {
	return keyRow{
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt, PreviousUntil: k.PreviousUntil,
		KeyHash: k.KeyHash.Reveal(), PreviousHash: k.PreviousHash.Reveal(),
		KeyPrefix: k.KeyPrefix, KeySuffix: k.KeySuffix, Label: k.Label, Scopes: k.Scopes,
		LineageIDs: k.LineageIDs, UserID: k.UserID,
	}
}

func (v *keyRow) key(id int64) access.Key {
	return access.Key{
		CreatedAt: v.CreatedAt, ExpiresAt: v.ExpiresAt, LastUsedAt: v.LastUsedAt, PreviousUntil: v.PreviousUntil,
		KeyHash: access.NewHash(v.KeyHash), PreviousHash: access.NewHash(v.PreviousHash),
		KeyPrefix: v.KeyPrefix, KeySuffix: v.KeySuffix, Label: v.Label, Scopes: v.Scopes,
		LineageIDs: v.LineageIDs, ID: id, UserID: v.UserID,
	}
}

// A token's key is its hash's text.
type tokenRow struct {
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Purpose     access.TokenPurpose
	Role        access.Role
	Subject     int64
	IssuedByKey int64
}

func toTokenRow(t *access.OneTimeToken) tokenRow {
	return tokenRow{
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Purpose: t.Purpose, Role: t.Role,
		Subject: t.Subject, IssuedByKey: t.IssuedByKey,
	}
}

func (v *tokenRow) token(hash []byte) access.OneTimeToken {
	return access.OneTimeToken{
		CreatedAt: v.CreatedAt, ExpiresAt: v.ExpiresAt, Hash: access.NewHash(string(hash)), Purpose: v.Purpose,
		Role: v.Role, Subject: v.Subject, IssuedByKey: v.IssuedByKey,
	}
}

// subjectKey is the tokens_by_subject key: the subject first, so a user's
// tokens share its ID as a prefix.
func subjectKey(subject int64, purpose access.TokenPurpose) []byte {
	return append(be64(subject), purpose...)
}

type attributionRow struct {
	At         time.Time
	Action     access.AttributionAction
	KeyLineage []int64
	UserID     int64
}

func (v *attributionRow) attribution(id int64) access.KeyAttribution {
	return access.KeyAttribution{At: v.At, Action: v.Action, KeyLineage: v.KeyLineage, ID: id, UserID: v.UserID}
}

type failureRow struct {
	LockedAt   time.Time
	Name       string
	Method     access.Method
	Count      int
	Trusted    bool
	Locked     bool
	Unreported bool
}

func toFailureRow(f *account.FailureCount) failureRow {
	return failureRow{
		LockedAt: f.LockedAt, Name: f.Name, Method: f.Method, Count: f.Count,
		Trusted: f.Trusted, Locked: f.Locked, Unreported: f.Unreported,
	}
}

func (v *failureRow) failure() account.FailureCount {
	return account.FailureCount{
		LockedAt: v.LockedAt, Name: v.Name, Method: v.Method, Trusted: v.Trusted,
		Count: v.Count, Locked: v.Locked, Unreported: v.Unreported,
	}
}

// failureKey lays a FailureRef out with each text length-prefixed, so no two
// refs share a key.
func failureKey(ref account.FailureRef) []byte {
	b := binary.AppendUvarint(nil, uint64(len(ref.Name)))
	b = append(b, ref.Name...)
	b = binary.AppendUvarint(b, uint64(len(ref.Method)))
	b = append(b, ref.Method...)
	if ref.Trusted {
		return append(b, 1)
	}
	return append(b, 0)
}
