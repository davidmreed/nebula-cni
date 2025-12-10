package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slackhq/nebula-cni/internal"
)

type logrusOutHack struct {
	prefix []byte
	io.Writer
}

// NewLogrusOutHack is a hack to add "podName" and "podIp" to all Logrus
// output. We can't just use WithFields because Nebula needs a *logrus.Logger
// and we can't just use a custom Formatter because Nebula changes
// the formatter during Main and after config reload.
func NewLogrusOutHack(podName, podIP string) (*logrusOutHack, error) {
	// Match the logging conventions of Nebula
	type prefix struct {
		PodName string `json:"podName"`
		PodIP   string `json:"podIp"`
	}

	raw, err := json.Marshal(prefix{
		PodName: podName,
		PodIP:   podIP,
	})
	if err != nil {
		return nil, err
	}

	raw[len(raw)-1] = ','

	return &logrusOutHack{
		prefix: raw,
		Writer: os.Stderr,
	}, nil
}

func (c *logrusOutHack) Write(p []byte) (n int, err error) {
	if len(p) > 0 && p[0] == '{' {
		n, err := c.Writer.Write(c.prefix)
		if err != nil {
			return n, err
		}
		return c.Writer.Write(p[1:])
	}

	return c.Writer.Write(p)
}

// returns a Writer that sends logs to the init container. Lossy, so it drops logs if
// it can't buffer them to send.
func startInitLogger(nebulaIP string) *chanWriter {
	ch := make(chan string, 64)
	cw := &chanWriter{ch: ch, finished: make(chan bool)}
	cr := &chanReader{ch: ch}

	go func() {
		defer close(cw.finished)

		url := fmt.Sprintf("http://%s/log", net.JoinHostPort(nebulaIP, internal.InitPort))

		resp, err := http.Head(url)
		if err != nil {
			l.WithError(err).Error("failed to HEAD /log, giving up on initLogger")
			cr.Discard()
			return
		}
		httpDiscardBody(resp)

		switch resp.StatusCode {
		case http.StatusOK, http.StatusNoContent:
			// Continue below
		default:
			l.WithField("url", url).
				WithField("init-version", resp.Header.Get(internal.HeaderVersion)).
				WithField("status", resp.StatusCode).
				Info("failed to HEAD /log, giving up on initLogger")
			cr.Discard()
			return
		}

		for !cw.closed {
			resp, err := http.Post(url, "text/plain", cr)
			if err != nil {
				l.WithError(err).Error("failed to log to init, continuing")
				continue
			}
			httpDiscardBody(resp)

			switch resp.StatusCode {
			case http.StatusOK, http.StatusNoContent:
				l.WithField("url", url).WithField("status", resp.StatusCode).Debug("Finished init logger POST")
			default:
				l.WithField("url", url).WithField("status", resp.StatusCode).Error("Finished init logger POST")
			}
		}
		l.Debug("Finished init logger reader")
	}()

	return cw
}

func getInitToken(nebulaIP string) (string, error) {
	url := fmt.Sprintf("http://%s/token", net.JoinHostPort(nebulaIP, internal.InitPort))

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusNotFound {
		httpDiscardBody(resp)
		return "", nil
	}

	if resp.StatusCode != http.StatusOK {
		msg, _ := httpReadBody(resp)
		return "", fmt.Errorf("GET /token: %d: %s", resp.StatusCode, msg)
	}

	token, err := httpReadBody(resp)
	if err != nil {
		return "", err
	}

	return string(bytes.TrimSpace(token)), nil
}

func httpDiscardBody(resp *http.Response) {
	_, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		l.WithError(err).Error("failed to read HTTP response body, continuing")
	}
	err = resp.Body.Close()
	if err != nil {
		l.WithError(err).Error("failed to close HTTP response body, continuing")
	}
}

func httpReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// chanWriter implements io.WriterCloser and drops writes if the chan is full
type chanWriter struct {
	ch       chan<- string
	closed   bool
	finished chan bool
}

func (c *chanWriter) Write(data []byte) (n int, err error) {
	select {
	case c.ch <- string(data):
	default:
		l.WithField("line", string(data)).Error("failed to write line to initLogger, channel full")
	}

	return len(data), nil
}

func (c *chanWriter) Close() error {
	if !c.closed {
		c.closed = true
		close(c.ch)
	}
	return nil
}

// CloseFlush closes the channel and then waits until the chanReader has
// finished consuming all messages (or a 1 second timeout)
func (c *chanWriter) CloseFlush() {
	c.Close()

	select {
	case <-c.finished:
		l.Debug("flushed initLogger")
	case <-time.After(1 * time.Second):
		l.Error("failed to flush initLogger after 1 second")
	}
}

// chanReader implements io.Reader and io.WriterTo, returning EOF when the
// channel is closed on the writer side
type chanReader struct {
	r  strings.Reader
	ch <-chan string
}

func (cr *chanReader) Read(p []byte) (int, error) {
	for {
		n, err := cr.r.Read(p)
		if n > 0 || err != io.EOF {
			return n, err
		}

		s, ok := <-cr.ch
		if !ok {
			return 0, io.EOF
		}
		cr.r.Reset(s)
	}
}

func (cr *chanReader) WriteTo(w io.Writer) (n int64, err error) {
	for {
		dn, err := cr.r.WriteTo(w)
		n += dn
		if err != nil {
			return n, err
		}

		s, ok := <-cr.ch
		if !ok {
			return n, nil
		}
		cr.r.Reset(s)
	}
}

// Discard reads and discards from the channel until closed
func (cr *chanReader) Discard() {
	for range cr.ch {
	}
}

// multiLogger logs both to the Nebula logrus and to the initLogger
type multiLogger struct {
	sl *slog.Logger
}

func (ml *multiLogger) Error(err error, msg string, args ...any) {
	ml.sl.Error(msg, "error", err)

	ll := l.WithError(err)
	for len(args) > 0 {
		ll = ll.WithField(args[0].(string), args[1])
		args = args[2:]
	}
	ll.Error(msg)
}

func (ml *multiLogger) Info(msg string, args ...any) {
	ml.sl.Info(msg, args...)

	var ll logrus.FieldLogger = l
	for len(args) > 0 {
		ll = ll.WithField(args[0].(string), args[1])
		args = args[2:]
	}
	ll.Info(msg)
}
