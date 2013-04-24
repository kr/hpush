package msg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	User byte = iota
	File
	Status
)

const (
	Success = iota
	Failure
)

func ReadFile(r io.Reader) (lr io.Reader, err error) {
	n, t, err := ReadHeader(r)
	if err == nil {
		lr = io.LimitReader(r, n)
	}
	if t != File {
		lr, err = nil, fmt.Errorf("expected file: %d", t)
	}
	return lr, err
}

func ReadFull(r io.Reader) (t byte, msg []byte, err error) {
	n, t, err := ReadHeader(r)
	if err != nil {
		return 0, nil, err
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	if err != nil {
		return 0, nil, err
	}
	return t, b, nil
}

func ReadHeader(r io.Reader) (n int64, t byte, err error) {
	br, ok := r.(io.ByteReader)
	if !ok {
		br = byteReader{r}
	}
	n, err = binary.ReadVarint(br)
	if err != nil {
		return
	}
	if n < 1 {
		err = errors.New("empty message")
		return
	}
	b := make([]byte, 1)
	_, err = r.Read(b)
	if err != nil {
		n = 0
		return
	}
	return n - 1, b[0], nil
}

func CopyN(w io.Writer, t byte, r io.Reader, n int64) error {
	v := make([]byte, binary.MaxVarintLen64)
	c := binary.PutVarint(v, (n + 1))
	_, err := w.Write(v[:c])
	if err != nil {
		return err
	}
	_, err = w.Write([]byte{t})
	if err != nil {
		return err
	}
	z, err := io.CopyN(w, r, n)
	if z == n {
		err = nil
	}
	return err
}

func Write(w io.Writer, t byte, p []byte) error {
	v := make([]byte, binary.MaxVarintLen64)
	c := binary.PutVarint(v, int64(len(p)+1))
	_, err := w.Write(v[:c])
	if err != nil {
		return err
	}
	_, err = w.Write([]byte{t})
	_, err = w.Write(p)
	return err
}

type lineWriter struct {
	w   io.Writer
	t   byte
	buf []byte
}

func LineWriter(w io.Writer, t byte) (lw io.Writer) {
	return &lineWriter{w: w, t: t}
}

func (w *lineWriter) Write(p []byte) (n int, err error) {
	w.buf = append(w.buf, p...)
	for {
		pos := bytes.IndexByte(w.buf, '\n')
		if pos < 0 {
			break
		}
		err = Write(w.w, w.t, w.buf[:pos+1])
		w.buf = w.buf[pos+1:]
	}
	return len(p), err
}

type byteReader struct {
	io.Reader
}

func (r byteReader) ReadByte() (c byte, err error) {
	b := make([]byte, 1)
	_, err = r.Read(b)
	return b[0], err
}
