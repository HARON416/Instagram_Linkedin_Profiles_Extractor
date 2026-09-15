package main

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestReplayInstagramRequestPreservesCapturedRequest(t *testing.T) {
	t.Parallel()

	const requestBody = "doc_id=fresh-doc-id&variables=fresh-variables"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if string(body) != requestBody {
			t.Errorf("body = %q, want %q", body, requestBody)
		}
		if got := request.Header.Get("X-Fb-Friendly-Name"); got != instagramProfileQueryName {
			t.Errorf("friendly name = %q", got)
		}
		if got := request.Header.Get("Cookie"); got != "sessionid=fresh-session" {
			t.Errorf("cookie = %q", got)
		}
		for header, expected := range map[string]string{
			"Accept-Language": "en-US,en;q=0.9",
			"Priority":        "u=1, i",
			"Sec-Fetch-Dest":  "empty",
			"Sec-Fetch-Mode":  "cors",
			"Sec-Fetch-Site":  "same-origin",
		} {
			if got := request.Header.Get(header); got != expected {
				t.Errorf("%s = %q, want %q", header, got, expected)
			}
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{"data":{"user":{"username":"example"}}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	responses := make(chan instagramProfileResponse, 1)
	replayInstagramRequest(client, instagramGraphQLRequest{
		method: http.MethodPost,
		url:    "https://www.instagram.com/api/graphql",
		headers: http.Header{
			"Accept-Language":    {"en-US,en;q=0.9"},
			"Content-Type":       {"application/x-www-form-urlencoded"},
			"Cookie":             {"sessionid=fresh-session"},
			"Priority":           {"u=1, i"},
			"Sec-Fetch-Dest":     {"empty"},
			"Sec-Fetch-Mode":     {"cors"},
			"Sec-Fetch-Site":     {"same-origin"},
			"X-Fb-Friendly-Name": {instagramProfileQueryName},
		},
		body: []byte(requestBody),
	}, responses)

	response := <-responses
	if response.statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.statusCode)
	}
	if username, err := instagramUsernameFromResponse(response.body); err != nil || username != "example" {
		t.Fatalf("response username = %q, err = %v", username, err)
	}
}

func TestCloneInstagramHeaders(t *testing.T) {
	t.Parallel()

	headers := cloneInstagramHeaders(proto.NetworkHeaders{
		"x-fb-friendly-name": gson.New(instagramProfileQueryName),
		"accept-encoding":    gson.New("br, gzip"),
		"content-length":     gson.New("123"),
	})

	if got := headers.Get("X-Fb-Friendly-Name"); got != instagramProfileQueryName {
		t.Fatalf("friendly name = %q", got)
	}
	if got := headers.Get("Accept-Encoding"); got != "br, gzip" {
		t.Fatalf("accept-encoding = %q, want captured value", got)
	}
	if got := headers.Get("Content-Length"); got != "123" {
		t.Fatalf("content-length = %q, want captured value", got)
	}
}

func TestFormatAcceptLanguage(t *testing.T) {
	t.Parallel()

	if got := formatAcceptLanguage("en-US,en,sw"); got != "en-US,en;q=0.9,sw;q=0.8" {
		t.Fatalf("accept-language = %q", got)
	}
}

func TestCompleteInstagramHeaders(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Accept-Language": {"en-KE,en;q=0.9"}}
	completeInstagramHeaders(nil, headers)

	for header, expected := range map[string]string{
		"Accept-Language": "en-KE,en;q=0.9",
		"Priority":        "u=1, i",
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "same-origin",
	} {
		if got := headers.Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
}

func TestDecodeNetworkResponseBody(t *testing.T) {
	t.Parallel()

	plain, err := decodeNetworkResponseBody(`{"plain":true}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(plain), `{"plain":true}`; got != want {
		t.Fatalf("plain body = %q, want %q", got, want)
	}

	decoded, err := decodeNetworkResponseBody("eyJiYXNlNjQiOnRydWV9", true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(decoded), `{"base64":true}`; got != want {
		t.Fatalf("decoded body = %q, want %q", got, want)
	}
}

func TestInstagramProfileGraphQLRequestMatcher(t *testing.T) {
	t.Parallel()

	request := &proto.NetworkRequest{
		URL: "https://www.instagram.com/api/graphql?doc_id=123",
		Headers: proto.NetworkHeaders{
			"x-fb-friendly-name": gson.New(instagramProfileQueryName),
		},
	}
	if !isInstagramProfileGraphQLRequest(request, proto.NetworkResourceTypeFetch) {
		t.Fatal("expected the profile GraphQL request to match")
	}
	if isInstagramProfileGraphQLRequest(request, proto.NetworkResourceTypeImage) {
		t.Fatal("did not expect an image request to match")
	}

	request.Headers["x-fb-friendly-name"] = gson.New("DifferentQuery")
	if isInstagramProfileGraphQLRequest(request, proto.NetworkResourceTypeFetch) {
		t.Fatal("did not expect a different GraphQL query to match")
	}
}
