//go:build !windows

package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/cplieger/access/account"
)

// Service is the account service a, for redeeming the tokens it mints.
func (a *Auth) Service() *account.Service { return a.svc }

// SetDiscovery makes every single sign-on discovery of the test go through
// client, retried after retry, until its cleanup.
func SetDiscovery(t *testing.T, client *http.Client, retry time.Duration) {
	prevClient, prevRetry := oidcClient, discoveryRetry
	oidcClient, discoveryRetry = client, retry
	t.Cleanup(func() { oidcClient, discoveryRetry = prevClient, prevRetry })
}

// ClientAddr is what access is told of r's client, behind the trusted proxies.
var ClientAddr = clientAddr
