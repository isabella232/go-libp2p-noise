package noise

import (
	"context"
	"golang.org/x/crypto/poly1305"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/sec"
)

type testMode int

const (
	readBufferGtEncMsg testMode = iota
	readBufferGtPlainText
	readBufferLtPlainText
)

var bcs = map[string]struct {
	m testMode
}{
	"readBuffer > encrypted message": {
		readBufferGtEncMsg,
	},
	"readBuffer > decrypted plaintext": {
		readBufferGtPlainText,
	},
	"readBuffer < decrypted plaintext": {
		readBufferLtPlainText,
	},
}

func makeTransport(b *testing.B) *Transport {
	b.Helper()

	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, 256)
	if err != nil {
		b.Fatal(err)
	}
	tpt, err := New(priv)
	if err != nil {
		b.Fatalf("error constructing transport: %v", err)
	}
	return tpt
}

type benchenv struct {
	*testing.B

	initTpt *Transport
	respTpt *Transport
	rndSrc  rand.Source
}

func setupEnv(b *testing.B) *benchenv {
	b.StopTimer()
	defer b.StartTimer()
	initTpt := makeTransport(b)
	respTpt := makeTransport(b)

	return &benchenv{
		B:       b,
		initTpt: initTpt,
		respTpt: respTpt,
		rndSrc:  rand.NewSource(42),
	}
}

func (b benchenv) connect(stopTimer bool) (*secureSession, *secureSession) {
	initConn, respConn := net.Pipe()

	if stopTimer {
		b.StopTimer()
		defer b.StartTimer()
	}

	var initSession sec.SecureConn
	var initErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		initSession, initErr = b.initTpt.SecureOutbound(context.TODO(), initConn, b.respTpt.localID)
	}()

	respSession, respErr := b.respTpt.SecureInbound(context.TODO(), respConn)
	<-done

	if initErr != nil {
		b.Fatal(initErr)
	}

	if respErr != nil {
		b.Fatal(respErr)
	}

	return initSession.(*secureSession), respSession.(*secureSession)
}

func drain(r io.Reader, done chan<- error, buf []byte) {
	_, err := io.Copy(&discardWithBuffer{buf, ioutil.Discard}, r)
	done <- err
}

type discardWithBuffer struct {
	buf []byte
	io.Writer
}

func (d *discardWithBuffer) ReadFrom(r io.Reader) (n int64, err error) {
	readSize := 0
	for {
		readSize, err = r.Read(d.buf)
		n += int64(readSize)
		if err != nil {
			if err == io.EOF {
				return n, nil
			}
			return
		}
	}
}

func sink(dst io.WriteCloser, src io.Reader, done chan<- error, buf []byte) {
	_, err := io.CopyBuffer(dst, src, buf)
	if err != nil {
		done <- err
	}
	done <- dst.Close()
}

func pipeRandom(src rand.Source, w io.WriteCloser, r io.Reader, n int64, writeBuffer,
	readBuffer []byte) error {
	rnd := rand.New(src)
	lr := io.LimitReader(rnd, n)

	writeCh := make(chan error, 1)
	readCh := make(chan error, 1)

	go sink(w, lr, writeCh, writeBuffer)
	go drain(r, readCh, readBuffer)

	writeDone := false
	readDone := false
	for !(readDone && writeDone) {
		select {
		case err := <-readCh:
			if err != nil && err != io.EOF {
				return err
			}
			readDone = true
		case err := <-writeCh:
			if err != nil && err != io.EOF {
				return err
			}
			writeDone = true
		}
	}

	return nil
}

func minimum(a, b int) int {
	if a <= b {
		return a
	}

	return b
}

func randInRange(min, max int) int {
	return rand.Intn(max-min) + min
}

func benchDataTransfer(b *benchenv, dataSize int64, m testMode) {
	var totalBytes int64
	var totalTime time.Duration

	plainTextBufs := make([][]byte, 62)
	rbufs := make(map[int][]byte)
	for i := 0; i < len(plainTextBufs); i++ {
		// plaintext will be 2 KB to 63 KB
		plainTextBufs[i] = make([]byte, (i+2)*1024)
		switch m {
		case readBufferGtEncMsg:
			rbufs[i] = make([]byte, len(plainTextBufs[i])+poly1305.TagSize+1)
		case readBufferGtPlainText:
			rbufs[i] = make([]byte, len(plainTextBufs[i])+1)
		case readBufferLtPlainText:
			rbufs[i] = make([]byte, len(plainTextBufs[i])-2)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		initSession, respSession := b.connect(true)

		start := time.Now()

		bufi := i % len(plainTextBufs)
		err := pipeRandom(b.rndSrc, initSession, respSession, dataSize, plainTextBufs[bufi], rbufs[bufi])
		if err != nil {
			b.Fatalf("error sending random data: %s", err)
		}
		elapsed := time.Since(start)
		totalTime += elapsed
		totalBytes += dataSize
	}
	bytesPerSec := float64(totalBytes) / totalTime.Seconds()
	b.ReportMetric(bytesPerSec, "bytes/sec")
}

type bc struct {
	plainTextChunkLen int64
	readBufferLen     int64
}

func BenchmarkTransfer1MB(b *testing.B) {
	for n, bc := range bcs {
		b.Run(n, func(b *testing.B) {
			benchDataTransfer(setupEnv(b), 1024*1024, bc.m)
		})
	}

}

func BenchmarkTransfer100MB(b *testing.B) {
	for n, bc := range bcs {
		b.Run(n, func(b *testing.B) {
			benchDataTransfer(setupEnv(b), 1024*1024*100, bc.m)
		})
	}
}

func BenchmarkTransfer500Mb(b *testing.B) {
	for n, bc := range bcs {
		b.Run(n, func(b *testing.B) {
			benchDataTransfer(setupEnv(b), 1024*1024*500, bc.m)
		})
	}
}

func (b benchenv) benchHandshake() {
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		i, r := b.connect(false)
		b.StopTimer()
		err := i.Close()
		if err != nil {
			b.Errorf("error closing session: %s", err)
		}
		err = r.Close()
		if err != nil {
			b.Errorf("error closing session: %s", err)
		}
		b.StartTimer()
	}
}

func BenchmarkHandshakeXX(b *testing.B) {
	env := setupEnv(b)
	env.benchHandshake()
}
