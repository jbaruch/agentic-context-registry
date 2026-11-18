package diagnostic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOpaqueErrorDisplay(t *testing.T) {
	t.Parallel()
	if OpaqueError(nil) != nil {
		t.Fatal("nil error changed")
	}
	for _, test := range []struct{ name, input, want string }{
		{"empty", "", ""},
		{"ordinary", "read café 😀: permission denied", "read café 😀: permission denied"},
		{"multiline", "yaml: unmarshal errors:\n  line 2: " + strings.Repeat("unknown field; ", 20), "yaml: unmarshal errors:\n  line 2: " + strings.Repeat("unknown field; ", 20)},
		{"exact budget", strings.Repeat("x", 16384), strings.Repeat("x", 16384)},
		{"over budget", strings.Repeat("x", 16385), strings.Repeat("x", 16384) + "...[truncated]"},
		{"crossing rune", strings.Repeat("x", 16383) + "é!", strings.Repeat("x", 16383) + "...[truncated]"},
		{"whole rune", strings.Repeat("x", 16380) + "😀!", strings.Repeat("x", 16380) + "😀...[truncated]"},
		{"invalid UTF-8", "bad\xfftext", "bad�text"},
		{"normalization crosses budget", strings.Repeat("x", 16383) + "\xff", strings.Repeat("x", 16383) + "...[truncated]"},
		{"controls left for consumer", "bad\n\x1b\u202e", "bad\n\x1b\u202e"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := errors.New(test.input)
			wrapped := OpaqueError(original)
			if wrapped.Error() != test.want || !utf8.ValidString(wrapped.Error()) {
				t.Fatalf("display differs: got %d bytes, want %d", len(wrapped.Error()), len(test.want))
			}
			if errors.Unwrap(wrapped) != original || original.Error() != test.input {
				t.Fatal("original error changed")
			}
		})
	}
}

func TestOpaqueErrorPreservesChains(t *testing.T) {
	t.Parallel()
	pathErr := &os.PathError{Op: "read", Path: strings.Repeat("p", 1<<20), Err: os.ErrPermission}
	urlErr := &url.Error{Op: "Get", URL: strings.Repeat("u", 1<<20), Err: context.Canceled}
	decoderErr := &json.UnmarshalTypeError{Value: strings.Repeat("v", 1<<20), Type: reflect.TypeFor[int]()}
	joined := errors.Join(
		fmt.Errorf("read package: %w; restore the package", OpaqueError(pathErr)),
		fmt.Errorf("request package: %w; retry", OpaqueError(urlErr)),
		fmt.Errorf("decode package: %w; repair JSON", OpaqueError(decoderErr)),
	)
	var gotPath *os.PathError
	var gotURL *url.Error
	var gotDecoder *json.UnmarshalTypeError
	if !errors.Is(joined, os.ErrPermission) || !errors.Is(joined, context.Canceled) ||
		!errors.As(joined, &gotPath) || gotPath != pathErr ||
		!errors.As(joined, &gotURL) || gotURL != urlErr ||
		!errors.As(joined, &gotDecoder) || gotDecoder != decoderErr {
		t.Fatal("wrapped/joined errors lost original identities")
	}
	if len(pathErr.Path) != 1<<20 || len(urlErr.URL) != 1<<20 || len(decoderErr.Value) != 1<<20 {
		t.Fatal("underlying error mutated")
	}
	if OpaqueError(urlErr).Error() != "context canceled" {
		t.Fatal("direct URL error must display only its cause")
	}
	composed := fmt.Errorf("ACR context: %w; ACR guidance", &url.Error{Op: "Get", URL: "https://example.test", Err: context.Canceled})
	if OpaqueError(composed).Error() != `ACR context: Get "https://example.test": context canceled; ACR guidance` {
		t.Fatal("display selection searched through a composed error")
	}
}
