package logging

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "debug")
	l.Info("login Basic YWxpY2U6c2VjcmV0cGFzc3dvcmQ= failed",
		"password", "hunter2",
		"x_auth_password", "abcd-efgh-ijkl-mnop",
		"note", "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJkaWQ6cGxjOnh5eiJ9.c2lnbmF0dXJl here",
		"err", errors.New("upstream said password=abcd-efgh-ijkl-mnop"),
		"query", "oauth_token=supersecret&count=20",
	)
	out := buf.String()
	for _, leak := range []string{"hunter2", "abcd-efgh-ijkl-mnop", "eyJhbGciOiJIUzI1NiJ9", "YWxpY2U6c2VjcmV0cGFzc3dvcmQ", "supersecret"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "count=20") {
		t.Errorf("non-secret query parameter was lost: %s", out)
	}
}
