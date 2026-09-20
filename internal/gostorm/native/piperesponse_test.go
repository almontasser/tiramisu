package native

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// The pipe carries file bytes. An error response must reach the reader as a failed read,
// never as content: ServeContent's "seeker can't seek" body decoded as video is exactly
// the picture breaking up for a few seconds.
func TestPipeResponseWriterFailsOnErrorStatus(t *testing.T) {
	pr, pw := io.Pipe()
	w := &PipeResponseWriter{writer: pw, header: make(http.Header)}

	go func() {
		// What http.Error does: status first, then the body.
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("seeker can't seek\n"))
		pw.Close()
	}()

	got, err := io.ReadAll(pr)
	if err == nil {
		t.Fatalf("read succeeded with %q, want an error", got)
	}
	if bytes.Contains(got, []byte("seeker can't seek")) {
		t.Fatalf("error body was served as file content: %q", got)
	}
}

// A normal range response must still stream through untouched.
func TestPipeResponseWriterPassesContent(t *testing.T) {
	pr, pw := io.Pipe()
	w := &PipeResponseWriter{writer: pw, header: make(http.Header)}

	want := []byte("\x00\x00\x00\x01video payload")
	go func() {
		w.WriteHeader(http.StatusPartialContent)
		w.Write(want)
		pw.Close()
	}()

	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
