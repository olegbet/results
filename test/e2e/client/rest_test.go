package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
)

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

type responseTransport struct{ response *http.Response }

func (r responseTransport) RoundTrip(*http.Request) (*http.Response, error) { return r.response, nil }

func TestRESTStatusAndBodyCleanup(t *testing.T) {
	for _, code := range []int{200, 401, 403, 404, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			b := &trackedBody{Reader: strings.NewReader(`{"results":[]}`)}
			c, err := NewRESTClient("https://example.test")
			if err != nil {
				t.Fatal(err)
			}
			rc := c.(*restClient)
			rc.httpClient.Transport = responseTransport{&http.Response{StatusCode: code, Body: b}}
			_, err = c.ListResults(context.Background(), &pb.ListResultsRequest{Parent: "ns-00"})
			if !b.closed {
				t.Error("response body left open")
			}
			if code == 200 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var statusErr *HTTPStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != code {
				t.Fatalf("missing typed status for %d", code)
			}
			if err.Error() != http.StatusText(code) {
				t.Error("legacy error text changed")
			}
		})
	}
}
