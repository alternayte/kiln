package guestproto

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStdout, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != FrameStdout {
		t.Fatalf("type %q, want %q", typ, FrameStdout)
	}
	if string(payload) != "hello\n" {
		t.Fatalf("payload %q", payload)
	}
}

func TestFrameEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameResult, nil); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != FrameResult || len(payload) != 0 {
		t.Fatalf("got type %q payload %q", typ, payload)
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	var hdr [5]byte
	hdr[0] = FrameStdout
	binary.BigEndian.PutUint32(hdr[1:], MaxFrame+1)
	if _, _, err := ReadFrame(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("ReadFrame accepted a frame over the limit")
	}
}

func TestWriteFrameRejectsOversize(t *testing.T) {
	if err := WriteFrame(&bytes.Buffer{}, FrameStdout, make([]byte, MaxFrame+1)); err == nil {
		t.Fatal("WriteFrame accepted a frame over the limit")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	req := Request{Op: OpExec, Cmd: []string{"echo", "hello"}, Cwd: "/work", Env: map[string]string{"A": "b"}, TimeoutSeconds: 5}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, FrameRequest, req); err != nil {
		t.Fatal(err)
	}
	var got Request
	typ, err := ReadJSON(&buf, &got)
	if err != nil {
		t.Fatal(err)
	}
	if typ != FrameRequest {
		t.Fatalf("type %q", typ)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("got %+v, want %+v", got, req)
	}
}

func TestNormalizeExec(t *testing.T) {
	req := Request{Op: OpExec, Cmd: []string{"echo"}}
	if e := NormalizeExec(&req); e != nil {
		t.Fatalf("NormalizeExec: %v", e)
	}
	if req.Cwd != "/" {
		t.Fatalf("cwd %q, want /", req.Cwd)
	}
	if req.TimeoutSeconds != ExecTimeoutDefault {
		t.Fatalf("timeout %d, want %d", req.TimeoutSeconds, ExecTimeoutDefault)
	}
}

func TestNormalizeExecRejects(t *testing.T) {
	cases := map[string]Request{
		"no cmd":       {Op: OpExec},
		"negative":     {Op: OpExec, Cmd: []string{"echo"}, TimeoutSeconds: -1},
		"over the cap": {Op: OpExec, Cmd: []string{"echo"}, TimeoutSeconds: ExecTimeoutMax + 1},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			req := req
			e := NormalizeExec(&req)
			if e == nil {
				t.Fatal("NormalizeExec accepted the request")
			}
			if e.Code != CodeInvalid {
				t.Fatalf("code %q, want %q", e.Code, CodeInvalid)
			}
		})
	}
}

func TestExecEnv(t *testing.T) {
	env := ExecEnv(map[string]string{"FOO": "bar", "PATH": "/custom"})
	joined := strings.Join(env, "\n") + "\n"
	for _, want := range []string{"FOO=bar", "HOME=/root", "PATH=/custom"} {
		if !strings.Contains(joined, want+"\n") {
			t.Fatalf("env does not contain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "PATH=/usr/local/sbin") {
		t.Fatalf("request PATH did not win:\n%s", joined)
	}
}
