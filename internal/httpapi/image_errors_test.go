package httpapi

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestImageFailureCodesPreserveReachableCause(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&provider.ImageFlowError{Stage: "generation", Submitted: true, Cause: io.ErrUnexpectedEOF}, 502, "image_stream_interrupted"},
		{&provider.ImageFlowError{Stage: "download", Submitted: true, Cause: io.ErrUnexpectedEOF}, 502, "image_delivery_failed"},
		{&provider.ImageFlowError{Stage: "poll", Submitted: true, Cause: &protocol.UpstreamError{Status: 504}}, 504, "image_generation_timeout"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, 502, "upstream_connection_failed"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}, 504, "upstream_connection_timeout"},
		{errors.Join(provider.ErrImageTextReply, &protocol.UpstreamError{Status: 422}), 422, "upstream_text_reply"},
	}
	for _, c := range cases {
		status, code, _ := imageErrorCode(c.err)
		if status != c.status || code != c.code {
			t.Fatalf("%v: got %d/%s, want %d/%s", c.err, status, code, c.status, c.code)
		}
	}
}
