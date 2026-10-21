package httputil

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinnedCredentialHeaders is the canonical header list providers-design.md
// documents; the matcher must never regress on these spellings.
var pinnedCredentialHeaders = []string{
	"Authorization", "X-Api-Key", "Api-Key", "Proxy-Authorization",
	"Cookie", "Set-Cookie", "X-Goog-Api-Key", "Openai-Organization",
}

// parseTestURL parses a URL a redaction test feeds into a secret matcher.
func parseTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestRedactHeaders(t *testing.T) {
	t.Parallel()

	t.Run("masks_every_credential_header", func(t *testing.T) {
		h := http.Header{}
		for _, k := range pinnedCredentialHeaders {
			h.Set(k, testAPIKey)
		}
		h.Set("Content-Type", "application/json")

		got := redactHeaders(h)
		for _, k := range pinnedCredentialHeaders {
			assert.Equal(t, []string{redactedMask}, got[k], k)
		}
		assert.Equal(t, "application/json", got.Get("Content-Type"))
	})

	t.Run("masks_custom_credential_names", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Custom-Token", testAPIKey)
		h.Set("My-Service-Key", testAPIKey)
		h.Set("Client-Secret", testAPIKey)

		got := redactHeaders(h)
		assert.Equal(t, []string{redactedMask}, got["X-Custom-Token"])
		assert.Equal(t, []string{redactedMask}, got["My-Service-Key"])
		assert.Equal(t, []string{redactedMask}, got["Client-Secret"])
	})

	t.Run("keeps_benign_headers_visible", func(t *testing.T) {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set("Anthropic-Version", "2023-06-01")
		h.Set("X-Session-Affinity", "sess-1")
		h.Set("Openai-Beta", "true")

		got := redactHeaders(h)
		assert.Equal(t, h, got)
	})

	t.Run("masks_non_canonical_keys", func(t *testing.T) {
		h := http.Header{"x-api-key": {testAPIKey}}

		got := redactHeaders(h)
		//nolint:staticcheck // a non-canonical key is the case under test
		assert.Equal(t, []string{redactedMask}, got["x-api-key"])
	})

	t.Run("collapses_multi_value_headers", func(t *testing.T) {
		h := http.Header{}
		h.Add("Authorization", "one")
		h.Add("Authorization", testAPIKey)

		got := redactHeaders(h)
		assert.Equal(t, []string{redactedMask}, got["Authorization"])
	})

	t.Run("does_not_mutate_the_original", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", testAPIKey)
		redactHeaders(h)
		assert.Equal(t, testAPIKey, h.Get("Authorization"))
	})
}

func TestRedactURL(t *testing.T) {
	t.Parallel()

	t.Run("nil_input", func(t *testing.T) {
		assert.Empty(t, redactURL(nil))
	})

	tests := []struct {
		name     string
		raw      string
		contains string
	}{
		{"api_key_param", "https://x.test/v1?api_key=" + testAPIKey, redactedQuery},
		{"key_param", "https://x.test/v1?key=" + testAPIKey, redactedQuery},
		{"access_token_param", "https://x.test/v1?access_token=" + testAPIKey, redactedQuery},
		{"compound_secret_param", "https://x.test/v1?client_secret=" + testAPIKey, redactedQuery},
		{"compound_refresh_param", "https://x.test/v1?refresh_token=" + testAPIKey, redactedQuery},
		{"unrelated_param_kept", "https://x.test/v1?model=opus", "model=opus"},
		{"no_query", "https://x.test/v1", "https://x.test/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			require.NoError(t, err)

			got := redactURL(u)
			assert.Contains(t, got, tc.contains)
			assert.NotContains(t, got, testAPIKey)
		})
	}
}

func TestRedactURLVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		contains string
	}{
		{"upper_case", "https://x.test/v1?API_KEY=" + testAPIKey, "API_KEY=redacted"},
		{"mixed_case", "https://x.test/v1?ApiKey=" + testAPIKey, "ApiKey=redacted"},
		{"no_separator", "https://x.test/v1?apikey=" + testAPIKey, "apikey=redacted"},
		{"dash_separator", "https://x.test/v1?api-key=" + testAPIKey, "api-key=redacted"},
		{"token_param", "https://x.test/v1?token=" + testAPIKey, "token=redacted"},
		{"auth_param", "https://x.test/v1?auth=" + testAPIKey, "auth=redacted"},
		{"secret_param", "https://x.test/v1?secret=" + testAPIKey, "secret=redacted"},
		{"userinfo_masked", "https://user:pw@" + "x.test/v1", "x.test/v1"},
		{"unrelated_param_kept", "https://x.test/v1?model=opus", "model=opus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			require.NoError(t, err)

			got := redactURL(u)
			assert.Contains(t, got, tc.contains)
			assert.NotContains(t, got, testAPIKey)
			assert.NotContains(t, got, "user:pw")
		})
	}
}

func TestCredentialName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"exact_key", "key", true},
		{"lowercase", "api_key", true},
		{"upper_case", "API_KEY", true},
		{"mixed_case", "ApiKey", true},
		{"dash_separator", "access-token", true},
		{"compound_secret", "client_secret", true},
		{"compound_refresh_token", "refresh_token", true},
		{"exact_authorization", "Authorization", true},
		{"exact_cookie", "Set-Cookie", true},
		{"prefix_match_not_enough", "keys", false},
		{"unrelated", "model", false},
		{"no_separator_boundary", "keyboard", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, credentialName(tc.key))
		})
	}

	t.Run("suffix_collision_over_masks", func(t *testing.T) {
		// monkey ends with key; over-masking a benign name is the safe direction
		assert.True(t, credentialName("monkey"))
	})
}

func TestRedactErrURL(t *testing.T) {
	t.Parallel()

	u, err := url.Parse("https://x.test/v1?api_key=" + testAPIKey)
	require.NoError(t, err)

	t.Run("rewrites_the_url_error", func(t *testing.T) {
		uerr := &url.Error{Op: "Post", URL: u.String(), Err: io.EOF}

		redactErrURL(uerr, u)
		assert.Equal(t, "Post \"https://x.test/v1?api_key=redacted\": "+io.EOF.Error(), uerr.Error())
	})

	t.Run("wrapped_url_error_reached", func(t *testing.T) {
		var redacted *url.Error
		wrapped := &retryableError{err: &url.Error{Op: "Post", URL: u.String(), Err: io.EOF}}

		redactErrURL(wrapped, u)
		require.ErrorAs(t, wrapped, &redacted)
		assert.NotContains(t, redacted.Error(), testAPIKey)
	})

	t.Run("other_errors_untouched", func(t *testing.T) {
		plain := io.EOF
		redactErrURL(plain, u)
		assert.Equal(t, io.EOF, plain)
	})
}

func TestScrubSecrets(t *testing.T) {
	t.Parallel()

	t.Run("replaces_known_values", func(t *testing.T) {
		body := []byte(`{"error":"invalid key ` + testAPIKey + ` provided"}`)

		got := string(scrubSecrets(body, []string{testAPIKey}))
		assert.JSONEq(t, `{"error":"invalid key [redacted] provided"}`, got)
	})

	t.Run("replaces_bearer_token_without_prefix", func(t *testing.T) {
		body := []byte(`error: token ` +
			strings.TrimPrefix("Bearer "+testAPIKey, "Bearer ") + ` rejected`)

		secrets := secretValues(http.Header{"Authorization": {"Bearer " + testAPIKey}}, nil)
		got := string(scrubSecrets(body, secrets))
		assert.NotContains(t, got, testAPIKey)
	})

	t.Run("longest_secret_applies_first", func(t *testing.T) {
		long := "sk-aaaaaaaaaaaaaaaa"
		body := []byte(`key: ` + long)

		got := string(scrubSecrets(body, []string{"sk-aaa", long}))
		assert.Equal(t, "key: "+redactedMask, got)
	})

	t.Run("short_literal_not_applied", func(t *testing.T) {
		body := []byte(`invalid id`)

		got := string(scrubSecrets(body, []string{"id"}))
		assert.Equal(t, "invalid id", got)
	})

	t.Run("alpha_only_literal_not_applied", func(t *testing.T) {
		body := []byte(`{"error":"password too short"}`)

		got := string(scrubSecrets(body, []string{"password"}))
		assert.Equal(t, string(body), got)
	})

	t.Run("literal_cannot_split_a_key", func(t *testing.T) {
		body := []byte(`key: sk-abcd1234efgh5678`)

		got := string(scrubSecrets(body, []string{"abcd1234"}))
		assert.Equal(t, "key: "+redactedMask, got)
	})

	t.Run("scrubs_multi_url_body", func(t *testing.T) {
		body := []byte(`failed for https://user:p%40ss@x.test/v1 and ` +
			`https://y.test/v2?key=` + testPlainKey)
		secrets := append(urlSecrets(parseTestURL(t, "https://user:p%40ss@x.test/v1")),
			urlSecrets(parseTestURL(t, "https://y.test/v2?key="+testPlainKey))...)

		got := string(scrubSecrets(body, secrets))
		assert.NotContains(t, got, "p%40ss")
		assert.NotContains(t, got, testPlainKey)
	})

	t.Run("masks_key_shaped_strings", func(t *testing.T) {
		tests := []struct {
			name string
			body string
		}{
			{"openai_shape", `{"message":"bad key sk-1234567890abcdefghijklmnop"}`},
			{"anthropic_shape", `{"message":"bad key sk-ant-1234567890abcdefghijklmnop"}`},
			{"google_shape", `{"message":"bad key AIza1234567890abcdefghijklmnopqrstuvwxy"}`},
			{"bearer_prefix", `{"message":"bad key Bearer 1234567890abcdefghijklmnop"}`},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assert.NotContains(t, string(scrubSecrets([]byte(tc.body), nil)), "1234567890")
			})
		}
	})

	t.Run("leaves_normal_text_alone", func(t *testing.T) {
		body := []byte(`{"error":"prompt is too long: 128000 > 200000"}`)

		assert.Equal(t, string(body), string(scrubSecrets(body, nil)))
	})

	t.Run("empty_secrets_ignored", func(t *testing.T) {
		body := []byte("no credentials here")

		assert.Equal(t, string(body), string(scrubSecrets(body, []string{""})))
	})
}

func TestExpandSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vals []string
		want []string
	}{
		{"drops_empty_values", []string{"", testAPIKey}, []string{testAPIKey}},
		{"sorts_longest_first", []string{testAPIKey, testPlainKey}, []string{testPlainKey, testAPIKey}},
		{"drops_short_values", []string{"abc", testAPIKey}, []string{testAPIKey}},
		{"drops_alpha_only_values", []string{"password"}, []string{}},
		{"splits_bearer_pair", []string{"Bearer " + testAPIKey},
			[]string{"Bearer " + testAPIKey, testAPIKey}},
		{"splits_lowercase_scheme", []string{"bearer " + testAPIKey},
			[]string{"bearer " + testAPIKey, testAPIKey}},
		{"splits_basic_pair", []string{"Basic dXNlcjpwYXNz"},
			[]string{"Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"}},
		{"splits_token_scheme", []string{"Token ghp_1234567890"},
			[]string{"Token ghp_1234567890", "ghp_1234567890"}},
		{"unfit_token_keeps_whole", []string{"Token abc"}, []string{"Token abc"}},
		{"bare_scheme_prefix_dropped", []string{"Bearer "}, []string{}},
		{"unknown_scheme_kept_whole", []string{"Negotiate abcdef"}, []string{"Negotiate abcdef"}},
		{"cookie_pair_not_split", []string{"sessionid=x; Path=/"}, []string{"sessionid=x; Path=/"}},
		{"scheme_without_space", []string{"BearerNoSpace"}, []string{"BearerNoSpace"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, expandSecrets(tc.vals))
		})
	}
}

func TestURLSecrets(t *testing.T) {
	t.Parallel()

	t.Run("nil_url", func(t *testing.T) {
		assert.Empty(t, urlSecrets(nil))
	})

	t.Run("no_credentials", func(t *testing.T) {
		assert.Empty(t, urlSecrets(parseTestURL(t, "https://x.test/v1?model=opus")))
	})

	t.Run("collects_credential_query_values", func(t *testing.T) {
		u := parseTestURL(t, "https://x.test/v1?api_key=abc&key=def&model=opus")
		assert.Equal(t, []string{"abc", "def"}, urlSecrets(u))
	})

	t.Run("collects_raw_and_decoded_forms", func(t *testing.T) {
		u := parseTestURL(t, "https://x.test/v1?api_key=a%2Bb%3D")
		assert.Equal(t, []string{"a%2Bb%3D", "a+b="}, urlSecrets(u))
	})

	t.Run("matches_encoded_names", func(t *testing.T) {
		u := parseTestURL(t, "https://x.test/v1?api%5Fkey=abc")
		assert.Equal(t, []string{"abc"}, urlSecrets(u))
	})

	t.Run("collects_userinfo", func(t *testing.T) {
		u := parseTestURL(t, "https://user:pw@x.test/v1")
		assert.Equal(t, []string{"user", "pw"}, urlSecrets(u))
	})

	t.Run("collects_encoded_userinfo", func(t *testing.T) {
		u := parseTestURL(t, "https://user:p%40ss@x.test/v1")
		assert.Equal(t, []string{"user", "p@ss", "p%40ss"}, urlSecrets(u))
	})

	t.Run("collects_password_only_userinfo", func(t *testing.T) {
		u := parseTestURL(t, "https://:"+testPlainKey+"@x.test/v1")
		assert.Equal(t, []string{testPlainKey}, urlSecrets(u))
	})

	t.Run("collects_username_without_password", func(t *testing.T) {
		u := parseTestURL(t, "https://"+testPlainKey+"@x.test/v1")
		assert.Equal(t, []string{testPlainKey}, urlSecrets(u))
	})

	t.Run("collects_repeated_values", func(t *testing.T) {
		u := parseTestURL(t, "https://x.test/v1?token=one&token=two")
		assert.Equal(t, []string{"one", "two"}, urlSecrets(u))
	})
}

func TestSecretValues(t *testing.T) {
	t.Parallel()

	t.Run("collects_credential_headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+testAPIKey)
		h.Set("X-Api-Key", testAPIKey)
		h.Set("Content-Type", "application/json")

		got := secretValues(h, nil)
		assert.ElementsMatch(t, []string{"Bearer " + testAPIKey, testAPIKey, testAPIKey}, got)
	})

	t.Run("collects_custom_credential_headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Custom-Token", testAPIKey)

		got := secretValues(h, nil)
		assert.Equal(t, []string{testAPIKey}, got)
	})

	t.Run("collects_url_query_credentials", func(t *testing.T) {
		got := secretValues(nil, parseTestURL(t, "https://x.test/v1?api_key="+testPlainKey))

		assert.Equal(t, []string{testPlainKey}, got)
	})

	t.Run("collects_userinfo_credentials", func(t *testing.T) {
		got := secretValues(nil, parseTestURL(t, "https://user:"+testPlainKey+"@x.test/v1"))
		assert.Equal(t, []string{testPlainKey}, got)
	})

	t.Run("merges_header_and_url_values", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Api-Key", testAPIKey)
		u := parseTestURL(t, "https://x.test/v1?access_token="+testPlainKey)

		got := secretValues(h, u)
		assert.ElementsMatch(t, []string{testAPIKey, testPlainKey}, got)
	})

	t.Run("splits_scheme_pairs", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Basic dXNlcjpwYXNz")

		got := secretValues(h, nil)
		assert.ElementsMatch(t, []string{"Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"}, got)
	})

	t.Run("empty_inputs", func(t *testing.T) {
		assert.Empty(t, secretValues(http.Header{}, nil))
	})
}

func TestRedactRawURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"drops_userinfo", "https://user:pw@x.test/v1", "https://x.test/v1"},
		{"last_at_wins", "https://a:b@c@d/p", "https://d/p"},
		{"masks_credential_query", "https://h/p?model=opus&key=" + testPlainKey + "#f",
			"https://h/p?model=opus&key=redacted#f"},
		{"encoded_name_masked", "https://h/p?api%5Fkey=" + testPlainKey, "https://h/p?api%5Fkey=redacted"},
		{"valueless_pair_kept", "https://h/p?key", "https://h/p?key"},
		{"no_scheme_masks_query", "/p?token=" + testPlainKey, "/p?token=redacted"},
		{"nothing_to_redact", "ht tp://x", "ht tp://x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, redactRawURL(tc.raw))
		})
	}
}

func TestRedactRawErrURL(t *testing.T) {
	t.Parallel()

	t.Run("parse_error_rewritten", func(t *testing.T) {
		raw := "https://user:" + testPlainKey + "%zz@x.test/v1"
		//nolint:staticcheck // an invalid escape is the case under test
		_, err := url.Parse(raw)
		require.Error(t, err)

		redactRawErrURL(err)
		assert.NotContains(t, err.Error(), testPlainKey)
	})

	t.Run("other_errors_untouched", func(t *testing.T) {
		plain := io.EOF
		redactRawErrURL(plain)
		assert.Equal(t, io.EOF, plain)
	})
}

func TestReadErrorBody(t *testing.T) {
	t.Parallel()

	t.Run("truncates_at_the_limit", func(t *testing.T) {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("a", errBodyLimit*2)))}
		assert.Len(t, readErrorBody(resp, nil), errBodyLimit)
	})

	t.Run("scrubs_before_the_limit_cut", func(t *testing.T) {
		pad := strings.Repeat("a", errBodyLimit-len(testAPIKey)/2)
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(pad + testAPIKey))}

		got := string(readErrorBody(resp, []string{testAPIKey}))
		assert.NotContains(t, got, testAPIKey[:len(testAPIKey)/2])
	})

	t.Run("scrubs_echoed_key", func(t *testing.T) {
		body := `{"error":"key ` + testAPIKey + ` invalid"}`
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

		got := readErrorBody(resp, []string{testAPIKey})
		assert.NotContains(t, string(got), testAPIKey)
		assert.Contains(t, string(got), redactedMask)
	})
}
