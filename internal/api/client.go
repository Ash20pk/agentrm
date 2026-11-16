package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"agentrm/internal/store"
)

// ErrDaemonDown means nothing answered on the socket.
var ErrDaemonDown = errors.New("the AgentRM daemon is not running")

// ErrVersion means the daemon speaks another protocol version.
var ErrVersion = errors.New("the AgentRM daemon is a different version")

// NeedsApproval is returned when a privileged request waits for a click in the app.
type NeedsApproval struct {
	ID      string
	Message string
}

func (e *NeedsApproval) Error() string { return e.Message }

func client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", store.SockPath())
		}},
	}
}

// Call sends req to path and decodes the answer into out (if not nil).
func Call(path string, req any, out any) error { return CallTimeout(5*time.Second, path, req, out) }

func CallTimeout(timeout time.Duration, path string, req any, out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, _ := http.NewRequest(http.MethodPost, "http://agentrm"+path, bytes.NewReader(b))
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(VersionHeader, strconv.Itoa(Version))
	resp, err := client(timeout).Do(hr)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return fmt.Errorf("the AgentRM daemon did not answer in %s", timeout)
		}
		return ErrDaemonDown
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	case http.StatusConflict:
		var e Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%w: %s", ErrVersion, e.Error)
	case http.StatusAccepted:
		var e Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &NeedsApproval{ID: e.ApprovalID, Message: e.Error}
	default:
		var e Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
}
