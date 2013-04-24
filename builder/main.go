package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"github.com/kr/hpush/msg"
	"github.com/kr/tarutil"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

const (
	selfPath = "/tmp/builder" // see ../main.go
	buildDir = "/tmp/build"
	cacheDir = "/tmp/cache"
	bpDir    = "/tmp/bp"
	compile  = bpDir + "/bin/compile"
)

// Communication with hpush proceeds as follows:
//   1. read slug url
//   2. read tarball
//   3. write user messages
//   4. write status
//   5. if success:
//      a. write slug
//      b. write procfile

func main() {
	signal.Notify(make(chan os.Signal), syscall.SIGHUP) // ignore

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		panic(err)
	}
	syscall.Dup2(int(devNull.Fd()), 0)
	syscall.Dup2(int(devNull.Fd()), 1)
	syscall.Dup2(int(devNull.Fd()), 2)

	err = os.Remove(selfPath)
	if err != nil {
		panic(err)
	}
	u, err := url.Parse(os.Args[1])
	if err != nil {
		panic(err)
	}
	addr, err := net.ResolveTCPAddr("tcp", u.Host)
	if err != nil {
		panic(err)
	}
	c, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		panic(err)
	}
	io.WriteString(c, "X "+u.Path+" HTTP/1.1\r\n\r\n")

	t, slugURL, err := msg.ReadFull(c)
	if err != nil {
		fail(c, err)
	}
	if t != msg.File {
		fail(c, fmt.Sprintf("wanted file, got %d\n", t))
	}

	r, err := msg.ReadFile(c)
	if err != nil {
		fail(c, err)
	}
	f, err := spool(r)
	if err != nil {
		fail(c, err)
	}
	msg.Write(c, msg.User, []byte(fmt.Sprintf("read tarball\n")))
	err = os.MkdirAll(buildDir, 0777)
	if err != nil {
		fail(c, err)
	}
	err = tarutil.ExtractAll(f, buildDir, 0)
	if err != nil {
		fail(c, err)
	}
	msg.Write(c, msg.User, []byte("extracted\n"))
	err = os.MkdirAll(cacheDir, 0777)
	if err != nil {
		fail(c, err)
	}

	bpurl := os.Getenv("BUILDPACK_URL")
	if bpurl == "" {
		errorExit(c, "no BUILDPACK_URL\n")
	}
	u, urlerr := url.Parse(bpurl)
	if urlerr == nil && u.Fragment != "" {
		bpurl = bpurl[:len(bpurl)-len(u.Fragment)-1]
	}
	msg.Write(c, msg.User, []byte("fetching buildpack\n"))
	msg.Write(c, msg.User, []byte(bpurl+"\n"))
	cmd := exec.Command("git", "clone", bpurl, bpDir)
	err = cmd.Run()
	if err != nil {
		msg.Write(c, msg.User, []byte(err.Error()+"\n"))
		errorExit(c, "failed to fetch buildpack\n")
	}
	if urlerr == nil && u.Fragment != "" {
		msg.Write(c, msg.User, []byte("git checkout "+u.Fragment+"\n"))
		cmd := exec.Command("git", "checkout", u.Fragment)
		cmd.Dir = bpDir
		err = cmd.Run()
		if err != nil {
			msg.Write(c, msg.User, []byte(err.Error()+"\n"))
			errorExit(c, "failed to check out ref: "+u.Fragment+"\n")
		}
	}
	err = os.RemoveAll(buildDir + "/.git")
	if err != nil {
		msg.Write(c, msg.User, []byte(err.Error()+"\n"))
		errorExit(c, "failed to clean .git dir\n")
	}

	msg.Write(c, msg.User, []byte("compiling\n"))
	cmd = exec.Command(compile, buildDir, cacheDir)
	cmd.Stdout = msg.LineWriter(c, msg.User)
	cmd.Stderr = cmd.Stdout
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		errorExit(c, "buildpack failed: "+ee.Error()+"\n")
	}
	if err != nil {
		fail(c, err)
	}
	msg.Write(c, msg.User, []byte("buildpack done\n"))
	slug, err := tempFile()
	if err != nil {
		fail(c, err)
	}
	tw := gzip.NewWriter(slug)
	err = entar(tw, buildDir, c)
	if err != nil {
		fail(c, err)
	}
	err = tw.Close()
	if err != nil {
		fail(c, err)
	}
	slug.Seek(0, 0)
	msg.Write(c, msg.User, []byte("slug built\n"))
	fi, err := slug.Stat()
	if err != nil {
		fail(c, err)
	}
	msg.Write(c, msg.User, []byte(fmt.Sprintf("slug %d bytes\n", fi.Size())))

	_ = slugURL

	procfile := readProcfile()
	if procfile == nil {
		fail(c, "could not read procfile")
	}
	msg.Write(c, msg.Status, []byte{msg.Success})
	err = msg.CopyN(c, msg.File, slug, fi.Size())
	if err != nil {
		panic(err)
	}
	err = msg.Write(c, msg.File, procfile)
	if err != nil {
		panic(err)
	}
	_, err = io.Copy(ioutil.Discard, c) // wait until other side closes
	if err != nil {
		panic(err)
	}
}

func fail(c net.Conn, err interface{}) {
	msg.Write(c, msg.User, []byte(fmt.Sprintf("%v\n", err)))
	errorExit(c, "internal error\n")
	panic(err)
}

func errorExit(c net.Conn, s string) {
	msg.Write(c, msg.User, []byte(s))
	msg.Write(c, msg.Status, []byte{msg.Failure})
	_, err := io.Copy(ioutil.Discard, c) // wait until other side closes
	if err != nil {
		panic(err)
	}
	os.Exit(1)
}

func readProcfile() []byte {
	b, _ := ioutil.ReadFile(buildDir + "/Procfile")
	return b
}

// read all of r into an unlinked temporary file
func spool(r io.Reader) (f *os.File, err error) {
	f, err = tempFile()
	if err != nil {
		return
	}
	_, err = io.Copy(f, r)
	if err != nil {
		f.Close()
		return nil, err
	}
	f.Seek(0, 0)
	return
}

func tempFile() (f *os.File, err error) {
	f, err = ioutil.TempFile(os.TempDir(), "temp")
	if err != nil {
		return
	}
	err = os.Remove(f.Name())
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func entar(w io.Writer, dir string, ww io.Writer) error {
	msg.Write(ww, msg.User, []byte("entar\n"))
	tw := tar.NewWriter(w)
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		hdr := new(tar.Header)
		hdr.Name = "./app" + path[len(dir):]
		hdr.Mode = int64(fi.Mode() & os.ModePerm)
		if fi.IsDir() {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = fi.Size()
		}
		if err = tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.IsDir() {
			var f *os.File
			f, err = os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
		}
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
