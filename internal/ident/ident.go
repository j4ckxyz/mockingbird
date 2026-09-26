// Package ident resolves Bluesky handles and DIDs through indigo's identity
// directory, with the SSRF-guarded HTTP client and a cache.
package ident

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Identity is a resolved account.
type Identity struct {
	DID    string
	Handle string // verified handle, or "handle.invalid"
	PDS    string // https origin of the PDS
}

// ErrNotFound means the handle or DID does not exist.
var ErrNotFound = errors.New("ident: account not found")

// ErrNoPDS means the DID document declares no usable PDS.
var ErrNoPDS = errors.New("ident: no PDS endpoint in DID document")

// Resolver resolves identities.
type Resolver interface {
	Resolve(ctx context.Context, identifier string) (*Identity, error)
	Purge(ctx context.Context, identifier string)
}

// Directory wraps an indigo Directory.
type Directory struct {
	dir identity.Directory
}

// Options configure the directory.
type Options struct {
	PLCURL     string
	HTTPClient *http.Client // guarded client for did:web and well-known
	PLCClient  *http.Client // client for the configured PLC directory
	HitTTL     time.Duration
	ErrTTL     time.Duration
	Capacity   int
	UserAgent  string
}

// New builds a caching, guarded directory.
func New(o Options) *Directory {
	base := &identity.BaseDirectory{
		PLCURL:     o.PLCURL,
		HTTPClient: *o.HTTPClient,
		PLCClient:  o.PLCClient,
		// Authoritative-nameserver fallback sends DNS queries to servers the
		// handle's owner chooses, which could be internal addresses. Off.
		TryAuthoritativeDNS: false,
		UserAgent:           o.UserAgent,
	}
	if o.Capacity == 0 {
		o.Capacity = 100_000
	}
	return &Directory{dir: identity.NewCacheDirectory(base, o.Capacity, o.HitTTL, o.ErrTTL, o.ErrTTL)}
}

// NewFromDirectory wraps an existing directory (tests).
func NewFromDirectory(d identity.Directory) *Directory { return &Directory{dir: d} }

// Resolve accepts a handle or DID. Handles are verified bidirectionally by
// the directory; a DID whose handle does not verify yields "handle.invalid".
func (d *Directory) Resolve(ctx context.Context, identifier string) (*Identity, error) {
	atid, err := syntax.ParseAtIdentifier(identifier)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a handle or DID", ErrNotFound, identifier)
	}
	id, err := d.dir.Lookup(ctx, atid)
	if err != nil {
		if errors.Is(err, identity.ErrHandleNotFound) || errors.Is(err, identity.ErrDIDNotFound) ||
			errors.Is(err, identity.ErrHandleMismatch) || errors.Is(err, identity.ErrInvalidHandle) {
			return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
		}
		return nil, err
	}
	pds, err := normalizePDS(id.PDSEndpoint())
	if err != nil {
		return nil, err
	}
	return &Identity{DID: id.DID.String(), Handle: id.Handle.String(), PDS: pds}, nil
}

// Purge drops cached state for identifier.
func (d *Directory) Purge(ctx context.Context, identifier string) {
	if atid, err := syntax.ParseAtIdentifier(identifier); err == nil {
		_ = d.dir.Purge(ctx, atid)
	}
}

// normalizePDS validates a PDS endpoint: an https origin with no path, query
// or credentials. The guarded client independently refuses internal
// addresses when it is dialled.
func normalizePDS(s string) (string, error) {
	if s == "" {
		return "", ErrNoPDS
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: malformed endpoint", ErrNoPDS)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("%w: bad scheme", ErrNoPDS)
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return "", fmt.Errorf("%w: endpoint has a path", ErrNoPDS)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// NormalizeIdentifier turns what a user typed at a 2009 login prompt into a
// handle or DID: strips "@", lowercases, and appends the default host to
// bare names ("alice" -> "alice.bsky.social").
func NormalizeIdentifier(s, defaultHost string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	if strings.HasPrefix(s, "did:") {
		return s
	}
	s = strings.ToLower(s)
	if !strings.Contains(s, ".") && defaultHost != "" && s != "" {
		s += "." + defaultHost
	}
	return s
}
