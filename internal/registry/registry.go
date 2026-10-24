// Package registry resolves the digest a tag currently points at, in an
// OCI/Docker v2 registry.
//
// The whole basis of "is there anything new?" - Tidewaiter never chases a tag
// blindly, it compares the digest the tag resolves to against the digest the
// container is running, exactly Podman's registry policy.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// Timeout bounds every registry request, its retries and its waits together:
// a registry that hangs must not hold up a pass, which leaves the container
// alone and tries again next time. Comfortably more than the most retries
// would wait - one second, then two, or two Retry-Afters of up to maxDelay -
// so the waits never use up the time the attempts need.
const Timeout = 60 * time.Second

// Auth is where credentials come from, keyed by registry host.
//
// It returns a ready X-Registry-Auth header value (base64 JSON) for a host, or
// "" for anonymous. The same value serves both the manifest request here and
// the Docker pull on the socket, so "where do creds live" stays in one place.
type Auth func(registry string) string

// Anonymous is no credentials for anyone.
func Anonymous(string) string { return "" }

// DefaultBackoff is the wait before the first retry, doubled for the next,
// and the wait when a 429 carries no usable Retry-After.
const DefaultBackoff = time.Second

// maxDelay is the longest Retry-After waited out. Docker Hub throttles
// anonymous pulls routinely, and names when it will answer; one that asks for
// longer is given up on at once, and the container left alone until a later
// pass.
const maxDelay = 10 * time.Second

// retries is how a registry request is retried: twice, after a transport
// error, a 429 or a 5xx.
func retries(clk clock.Clock) httpx.RetryPolicy {
	return httpx.RetryPolicy{Retries: 2, BaseDelay: DefaultBackoff, MaxDelay: maxDelay, Clock: clk}
}

// Ask for the shapes a digest lives behind: v2 manifests, manifest lists and
// their OCI equivalents. Without this a registry may hand back a v1 manifest
// with a different digest. A multi-arch image answers with its index's
// digest, which is what Docker records in RepoDigests after a pull - so the
// two compare without ever choosing a platform here.
var accept = strings.Join([]string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}, ", ")

// Client talks to registries over an http.RoundTripper.
//
// It resolves a manifest digest with a HEAD (falling back to GET) against the
// manifests endpoint, reading the Docker-Content-Digest header - the digest
// the registry itself vouches for, so it matches what a pull would land and
// what Docker records in RepoDigests.
//
// Every request it makes - manifests and tokens alike - is retried as retries
// says, its backoffs and Retry-Afters waited out on the client's clock.
type Client struct {
	http *http.Client
	auth Auth
}

// New is a client over transport, with credentials from auth (nil for
// anonymous) and retries waited out on clk.
func New(transport http.RoundTripper, auth Auth, clk clock.Clock) *Client {
	if auth == nil {
		auth = Anonymous
	}
	retrying := httpx.Decorate(transport, httpx.Retrying(retries(clk)))
	return &Client{http: &http.Client{Transport: retrying, Timeout: Timeout}, auth: auth}
}

// Digest is the manifest digest ref's tag points at now.
func (c *Client) Digest(ctx context.Context, ref string) (model.Digest, error) {
	digest, err := c.digest(ctx, ParseRef(ref))
	if err != nil {
		return "", fmt.Errorf("resolving digest for %s failed: %w", ref, err)
	}
	return digest, nil
}

func (c *Client) digest(ctx context.Context, image Ref) (model.Digest, error) {
	target := "https://" + image.Registry + "/v2/" + image.Repository + "/manifests/" + image.Reference

	// Docker Hub (and any token-auth registry) answers an anonymous manifest
	// request with 401 and a Bearer challenge naming where to get a token.
	// Fetch one and retry - the standard registry auth dance.
	ask := func(method string) (*http.Response, error) {
		response, err := c.manifest(ctx, image, target, method, "")
		if err != nil || response.StatusCode != http.StatusUnauthorized {
			return response, err
		}
		token, err := c.tokenFor(ctx, response, image)
		if err != nil {
			return nil, err
		}
		return c.manifest(ctx, image, target, method, token)
	}

	response, err := ask(http.MethodHead)
	if err != nil {
		return "", err
	}
	// A few registries do not answer HEAD on manifests; fall back to GET.
	if response.StatusCode == http.StatusMethodNotAllowed {
		drain(response)
		if response, err = ask(http.MethodGet); err != nil {
			return "", err
		}
	}
	if err := httpx.Check(response); err != nil {
		return "", err
	}
	defer drain(response)
	digest := response.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", errors.New("registry returned no Docker-Content-Digest header")
	}
	return model.Digest(digest), nil
}

// manifest is one manifest request.
func (c *Client) manifest(ctx context.Context, image Ref, target, method, bearer string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	} else if basic := basicAuth(c.auth(image.Registry)); basic != "" {
		request.Header.Set("Authorization", basic)
	}
	return c.do(request)
}

// do is a request, with the retries behind it; a 429 still there after them
// is an error saying so, which leaves the container alone this pass.
func (c *Client) do(request *http.Request) (*http.Response, error) {
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusTooManyRequests {
		drain(response)
		if after := response.Header.Get("Retry-After"); after != "" {
			return nil, fmt.Errorf("registry rate-limited %s (Retry-After: %s)", request.URL, after)
		}
		return nil, fmt.Errorf("registry rate-limited %s", request.URL)
	}
	return response, nil
}

// tokenFor follows a 401's Bearer challenge to a token, or "" when the
// challenge is not one we can follow (the retry then fails as it would have).
//
// The WWW-Authenticate header names the realm, service and scope; the token
// endpoint hands back a bearer token for exactly that scope. Anonymous pulls
// of public images work because the token endpoint grants a scoped anonymous
// token - no credentials needed for the common case.
func (c *Client) tokenFor(ctx context.Context, challenge *http.Response, image Ref) (string, error) {
	drain(challenge)
	header := challenge.Header.Get("WWW-Authenticate")
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return "", nil
	}
	params := parseChallenge(header[7:])
	if params["realm"] == "" {
		return "", nil
	}
	realm, err := url.Parse(params["realm"])
	if err != nil {
		return "", fmt.Errorf("unusable token realm %q: %w", params["realm"], err)
	}
	query := realm.Query()
	if params["service"] != "" {
		query.Set("service", params["service"])
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + image.Repository + ":pull"
	}
	query.Set("scope", scope)
	realm.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if basic := basicAuth(c.auth(image.Registry)); basic != "" {
		request.Header.Set("Authorization", basic)
	}
	response, err := c.do(request)
	if err != nil {
		return "", err
	}
	if err := httpx.Check(response); err != nil {
		return "", err
	}
	defer drain(response)
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("reading the token from %s: %w", realm.Host, err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

func drain(response *http.Response) {
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// parseChallenge reads the realm="...",service="...",scope="..." of a Bearer
// challenge.
func parseChallenge(body string) map[string]string {
	params := map[string]string{}
	for _, match := range challengeParam.FindAllStringSubmatch(body, -1) {
		params[match[1]] = match[2]
	}
	return params
}

// Ref is an image reference, split the way the registry API wants it.
type Ref struct {
	Registry   string
	Repository string
	// A tag or a digest.
	Reference string
}

// Docker's default registry is Docker Hub, under an odd pair of hostnames:
// refs name docker.io but the API lives at registry-1.docker.io, and a bare
// `nginx` means `library/nginx`. Normalising both here keeps the rest simple.
const DockerHub = "registry-1.docker.io"

var dockerHubAliases = map[string]bool{"docker.io": true, "index.docker.io": true, DockerHub: true}

// ParseRef splits an image reference into registry, repository and tag or
// digest.
func ParseRef(ref string) Ref {
	remainder := ref
	registry := DockerHub

	// A registry host is the part before the first slash *if* it looks like a
	// host (has a dot or colon, or is localhost) - otherwise the whole thing
	// is a Hub repository like `library/nginx` with no registry prefix.
	if first, rest, found := strings.Cut(remainder, "/"); found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		registry = first
		if dockerHubAliases[first] {
			registry = DockerHub
		}
		remainder = rest
	}

	// A digest follows `@`; a tag follows the last `:` (any registry:port is
	// already gone).
	repository, reference := remainder, "latest"
	if at := strings.Index(remainder, "@"); at != -1 {
		repository, reference = remainder[:at], remainder[at+1:]
	} else if colon := strings.LastIndex(remainder, ":"); colon != -1 {
		repository, reference = remainder[:colon], remainder[colon+1:]
	}

	// A single-segment repository on Docker Hub lives under `library/`.
	if registry == DockerHub && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return Ref{Registry: registry, Repository: repository, Reference: reference}
}

// basicAuth turns a stored X-Registry-Auth value back into a Basic
// Authorization header, or "" when it has none.
//
// The credential is kept as the base64 JSON Docker's socket wants; a direct
// registry request needs Basic instead, so the username and password are
// decoded back out. An identity token has no Basic form, so it is skipped
// (the Bearer path handles those registries).
func basicAuth(xRegistryAuth string) string {
	if xRegistryAuth == "" {
		return ""
	}
	data, err := base64.StdEncoding.DecodeString(xRegistryAuth)
	if err != nil {
		return ""
	}
	var credential struct {
		Username string  `json:"username"`
		Password *string `json:"password"`
	}
	if json.Unmarshal(data, &credential) != nil || credential.Username == "" || credential.Password == nil {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(credential.Username+":"+*credential.Password))
}

// DockerConfigAuth is an Auth built from ~/.docker/config.json, as read.
//
// Reads the `auths` map: each entry's `auth` is base64 `user:password`, which
// Docker's socket wants re-encoded as base64 JSON {username, password,
// serveraddress} for X-Registry-Auth. An `identitytoken` (from `docker login`
// with a token) is passed through as {identitytoken}. A config that cannot be
// read or understood, or a registry with no entry, means anonymous - the
// common homelab case.
func DockerConfigAuth(read func() ([]byte, error)) Auth {
	data, err := read()
	if err != nil {
		return Anonymous
	}
	var config struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if json.Unmarshal(data, &config) != nil {
		return Anonymous
	}
	byRegistry := map[string]string{}
	for host, entry := range config.Auths {
		server := normaliseHost(host)
		var value any
		switch {
		case entry.IdentityToken != "":
			value = map[string]string{"identitytoken": entry.IdentityToken, "serveraddress": server}
		case entry.Auth != "":
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			username, password, found := strings.Cut(string(decoded), ":")
			if err != nil || !found {
				continue
			}
			value = map[string]string{"username": username, "password": password, "serveraddress": server}
		default:
			continue
		}
		encoded, _ := json.Marshal(value)
		byRegistry[server] = base64.StdEncoding.EncodeToString(encoded)
	}
	return func(registry string) string { return byRegistry[normaliseHost(registry)] }
}

// normaliseHost treats the several hostnames Docker Hub is stored under as
// one, and drops the scheme and path some config entries carry.
func normaliseHost(host string) string {
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host, _, _ = strings.Cut(host, "/")
	if dockerHubAliases[host] {
		return DockerHub
	}
	return host
}
