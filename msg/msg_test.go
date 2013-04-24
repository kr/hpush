package msg

import (
	"bytes"
	"testing"
)

func TestMsg(t *testing.T) {
	b := new(bytes.Buffer)
	Write(b, Status, []byte{Failure})
	g, p, err := ReadFull(b)
	if err != nil {
		t.Error("err w nil, g %d", err)
	}
	if g != Status {
		t.Error("type w %d, g %d", Status, g)
	}
	if len(p) != 1 {
		t.Fatal("len w %d, g %d", 1, len(p))
	}
	if p[0] != Failure {
		t.Fatal("val w %d, g %d", Failure, p[0])
	}
}
