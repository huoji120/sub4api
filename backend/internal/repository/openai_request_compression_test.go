package repository

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type codexCompressionLoopbackTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (tr codexCompressionLoopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	local := req.Clone(req.Context())
	local.URL.Scheme = tr.target.Scheme
	local.URL.Host = tr.target.Host
	local.Host = tr.target.Host
	return tr.base.RoundTrip(local)
}

func TestCodexResponsesCompressionWireContract(t *testing.T) {
	payload := []byte(`{"model":"codex-test","stream":true,"input":"` + strings.Repeat("request content ", 256) + `"}`)
	for _, tc := range []struct {
		name         string
		target       string
		encoding     string
		wantEncoding string
	}{
		{"codex_responses", "https://chatgpt.com/backend-api/codex/responses", "", "zstd"},
		{"legacy_compact", "https://chatgpt.com/backend-api/codex/responses/compact", "", ""},
		{"public_api", "https://api.openai.com/v1/responses", "", ""},
		{"custom_upstream", "https://relay.example/backend-api/codex/responses", "", ""},
		{"existing_encoding", "https://chatgpt.com/backend-api/codex/responses", "identity", "zstd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type observation struct {
				body     []byte
				encoding string
				length   int64
				err      error
			}
			seen := make(chan observation, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				seen <- observation{body, r.Header.Get("Content-Encoding"), r.ContentLength, err}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
			}))
			defer srv.Close()
			target, err := url.Parse(srv.URL)
			require.NoError(t, err)
			client := &http.Client{Transport: codexCompressionLoopbackTransport{srv.Client().Transport, target}}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, tc.target, bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
			}
			for range 2 {
				resp, err := doUpstreamRequest(client, req)
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				got := <-seen
				require.NoError(t, got.err)
				require.Equal(t, tc.wantEncoding, got.encoding)
				require.Equal(t, int64(len(got.body)), got.length)
				decoded := got.body
				if got.encoding == "zstd" {
					decoder, err := zstd.NewReader(nil)
					require.NoError(t, err)
					decoded, err = decoder.DecodeAll(got.body, nil)
					decoder.Close()
					require.NoError(t, err)
					require.Less(t, len(got.body), len(payload))
				}
				require.Equal(t, payload, decoded)
				require.Equal(t, tc.encoding, req.Header.Get("Content-Encoding"), "transport must not mutate the semantic request headers")
				req.Body, err = req.GetBody()
				require.NoError(t, err)
			}
			_ = req.Body.Close()
		})
	}
}
