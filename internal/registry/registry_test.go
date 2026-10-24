package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/await"
	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
)

func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		ref  string
		want Ref
	}{
		// A bare name is a Docker Hub library image on :latest.
		{"nginx", Ref{DockerHub, "library/nginx", "latest"}},
		{"danielbodart/portical:1.2.3", Ref{DockerHub, "danielbodart/portical", "1.2.3"}},
		{"registry.example.com:5000/team/app:2", Ref{"registry.example.com:5000", "team/app", "2"}},
		{"docker.io/library/nginx:1.27", Ref{DockerHub, "library/nginx", "1.27"}},
		{"localhost/app", Ref{"localhost", "app", "latest"}},
		{"nginx@sha256:abc", Ref{DockerHub, "library/nginx", "sha256:abc"}},
	} {
		if got := ParseRef(c.ref); got != c.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", c.ref, got, c.want)
		}
	}
}

func client(handler http.HandlerFunc, auth Auth) (*Client, *clock.Instant) {
	waits := clock.NewInstant(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
	return New(httpx.HandlerTransport(handler), auth, waits), waits
}

func digestOf(digest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", digest)
	}
}

func TestReturnsTheDockerContentDigestFromAHeadOfTheManifest(t *testing.T) {
	var seen *http.Request
	registry, _ := client(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		digestOf("sha256:manifest")(w, r)
	}, nil)
	digest, err := registry.Digest(context.Background(), "nginx:1.27")
	if err != nil || digest != "sha256:manifest" {
		t.Fatalf("got %q, %v", digest, err)
	}
	if seen.Method != http.MethodHead || seen.Host != DockerHub || seen.URL.Path != "/v2/library/nginx/manifests/1.27" {
		t.Errorf("asked %s %s%s", seen.Method, seen.Host, seen.URL.Path)
	}
	// The index types too, so a multi-arch image answers with the digest Docker
	// records in RepoDigests.
	if accept := seen.Header.Get("Accept"); !strings.Contains(accept, "application/vnd.oci.image.index.v1+json") ||
		!strings.Contains(accept, "application/vnd.docker.distribution.manifest.list.v2+json") {
		t.Errorf("accepts %q", accept)
	}
}

func TestFollowsA401BearerChallengeToATokenThenRetries(t *testing.T) {
	var tokenAsked string
	registry, _ := client(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "auth.example":
			tokenAsked = r.URL.RawQuery
			json.NewEncoder(w).Encode(map[string]string{"token": "tok123"})
		case r.Header.Get("Authorization") == "":
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",service="reg",scope="repository:library/nginx:pull"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case r.Header.Get("Authorization") == "Bearer tok123":
			digestOf("sha256:authed")(w, r)
		default:
			http.Error(w, "wrong token "+r.Header.Get("Authorization"), http.StatusForbidden)
		}
	}, nil)
	digest, err := registry.Digest(context.Background(), "nginx:1.27")
	if err != nil || digest != "sha256:authed" {
		t.Fatalf("got %q, %v", digest, err)
	}
	if tokenAsked != "scope=repository%3Alibrary%2Fnginx%3Apull&service=reg" {
		t.Errorf("asked for a token with %q", tokenAsked)
	}
}

func TestFallsBackToGetWhenTheRegistryWillNotAnswerHead(t *testing.T) {
	registry, _ := client(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		digestOf("sha256:got")(w, r)
	}, nil)
	if digest, err := registry.Digest(context.Background(), "nginx"); err != nil || digest != "sha256:got" {
		t.Errorf("got %q, %v", digest, err)
	}
}

func TestSendsStoredCredentialsAsBasicAuth(t *testing.T) {
	var authorization string
	registry, _ := client(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		digestOf("sha256:private")(w, r)
	}, DockerConfigAuth(config(t, map[string]any{"auths": map[string]any{
		"ghcr.io": map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("alice:secret"))},
	}})))
	if _, err := registry.Digest(context.Background(), "ghcr.io/alice/app:1"); err != nil {
		t.Fatal(err)
	}
	if authorization != "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")) {
		t.Errorf("sent %q", authorization)
	}
}

func TestAMissingDigestHeaderIsAnErrorNamingTheRef(t *testing.T) {
	registry, _ := client(func(http.ResponseWriter, *http.Request) {}, nil)
	if _, err := registry.Digest(context.Background(), "nginx:1.27"); err == nil || !strings.Contains(err.Error(), "nginx:1.27") {
		t.Errorf("got %v", err)
	}
}

func TestARegistryRefusalCarriesItsWords(t *testing.T) {
	registry, _ := client(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "manifest unknown", http.StatusNotFound)
	}, nil)
	_, err := registry.Digest(context.Background(), "nginx:9")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("got %v", err)
	}
}

func rateLimitedOnce(retryAfter string) http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		digestOf("sha256:after-wait")(w, r)
	}
}

func TestWaitsOutA429WithRetryAfterThenSucceeds(t *testing.T) {
	registry, waits := client(rateLimitedOnce("3"), nil)
	digest, err := registry.Digest(context.Background(), "nginx:1.27")
	if err != nil || digest != "sha256:after-wait" {
		t.Fatalf("got %q, %v", digest, err)
	}
	if want := []time.Duration{3 * time.Second}; !slices.Equal(waits.Waits(), want) {
		t.Errorf("waited %v, want %v", waits.Waits(), want)
	}
}

func TestUsesADefaultBackoffWhenA429CarriesNoRetryAfter(t *testing.T) {
	registry, waits := client(rateLimitedOnce(""), nil)
	if _, err := registry.Digest(context.Background(), "nginx:1.27"); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{DefaultBackoff}; !slices.Equal(waits.Waits(), want) {
		t.Errorf("waited %v, want %v", waits.Waits(), want)
	}
}

func TestRetriesARegistryErrorBackingOffTwiceAsLongEachTime(t *testing.T) {
	calls := 0
	registry, waits := client(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		digestOf("sha256:third-time")(w, r)
	}, nil)
	if digest, err := registry.Digest(context.Background(), "nginx:1.27"); err != nil || digest != "sha256:third-time" {
		t.Fatalf("got %q, %v", digest, err)
	}
	if want := []time.Duration{DefaultBackoff, 2 * DefaultBackoff}; !slices.Equal(waits.Waits(), want) {
		t.Errorf("waited %v, want %v", waits.Waits(), want)
	}
}

// Waiting it out would hold the pass, and every container after it, for as
// long as the registry liked: the next pass asks again instead.
func TestARetryAfterLongerThanItWillWaitIsRateLimitedAtOnce(t *testing.T) {
	registry, waits := client(rateLimitedOnce("3600"), nil)
	_, err := registry.Digest(context.Background(), "nginx:1.27")
	if err == nil || !strings.Contains(err.Error(), "rate-limited") || !strings.Contains(err.Error(), "Retry-After: 3600") {
		t.Errorf("got %v", err)
	}
	if len(waits.Waits()) != 0 {
		t.Errorf("waited %v", waits.Waits())
	}
}

func TestGivesUpWithARateLimitedErrorAfterPersistent429s(t *testing.T) {
	calls := 0
	registry, _ := client(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}, nil)
	if _, err := registry.Digest(context.Background(), "nginx:1.27"); err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("got %v", err)
	}
	if calls != 3 {
		t.Errorf("asked %d times, want the first and two retries", calls)
	}
}

func TestABackoffEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
	registry := New(httpx.HandlerTransport(rateLimitedOnce("5")), nil, stopped)
	failed := make(chan error, 1)
	go func() {
		_, err := registry.Digest(ctx, "nginx")
		failed <- err
	}()
	if err := stopped.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-failed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	case <-time.After(await.Timeout):
		t.Fatal("the backoff outlived its context")
	}
}

func config(t *testing.T, value any) func() ([]byte, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(value)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return func() ([]byte, error) { return os.ReadFile(path) }
}

func decoded(t *testing.T, value string) map[string]string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestDockerConfigAuthEncodesAUserPassEntryAsXRegistryAuth(t *testing.T) {
	auth := DockerConfigAuth(config(t, map[string]any{"auths": map[string]any{
		"https://index.docker.io/v1/": map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("alice:se:cret"))},
	}}))
	value := auth(DockerHub)
	if value == "" {
		t.Fatal("no credential for Docker Hub")
	}
	if got := decoded(t, value); got["username"] != "alice" || got["password"] != "se:cret" || got["serveraddress"] != DockerHub {
		t.Errorf("got %v", got)
	}
	if auth("docker.io") != value {
		t.Error("docker.io is Docker Hub too")
	}
}

func TestDockerConfigAuthPassesAnIdentityTokenThrough(t *testing.T) {
	auth := DockerConfigAuth(config(t, map[string]any{"auths": map[string]any{
		"ghcr.io": map[string]string{"identitytoken": "idt"},
	}}))
	if got := decoded(t, auth("ghcr.io")); got["identitytoken"] != "idt" {
		t.Errorf("got %v", got)
	}
	// No Basic form: the Bearer path serves those registries.
	if basicAuth(auth("ghcr.io")) != "" {
		t.Error("an identity token has no Basic form")
	}
}

func TestAMissingOrUnreadableConfigMeansAnonymousEverywhere(t *testing.T) {
	missing := DockerConfigAuth(func() ([]byte, error) { return nil, os.ErrNotExist })
	garbled := DockerConfigAuth(func() ([]byte, error) { return []byte("{not json"), nil })
	for _, auth := range []Auth{missing, garbled} {
		if got := auth(DockerHub); got != "" {
			t.Errorf("got %q", got)
		}
	}
}
