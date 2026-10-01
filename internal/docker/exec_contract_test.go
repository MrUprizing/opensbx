package docker

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type execDeadlineTransport struct {
	base      http.RoundTripper
	deadlines chan time.Time
}

func (t *execDeadlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/containers/container-1/exec") {
		if deadline, ok := request.Context().Deadline(); ok {
			t.deadlines <- deadline
		}
	}
	return t.base.RoundTrip(request)
}

func TestExecWithStdinUsesTwoMinuteDeadlineAndHonorsShorterCallerDeadline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		callerTimeout time.Duration
	}{
		{name: "internal deadline"},
		{name: "shorter caller deadline", callerTimeout: 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.Proxy = nil
			t.Cleanup(base.CloseIdleConnections)
			transport := &execDeadlineTransport{base: base, deadlines: make(chan time.Time, 1)}
			dc, _ := newDockerFixtureWithRoundTripper(t, transport)

			parent := context.Background()
			var cancel context.CancelFunc = func() {}
			var callerDeadline time.Time
			if tc.callerTimeout > 0 {
				parent, cancel = context.WithTimeout(parent, tc.callerTimeout)
				callerDeadline, _ = parent.Deadline()
			}
			defer cancel()
			started := time.Now()
			_, err := dc.execWithStdin(parent, "container-1", []string{"cat"}, nil)
			require.NoError(t, err)

			select {
			case actual := <-transport.deadlines:
				if tc.callerTimeout > 0 {
					require.True(t, actual.Equal(callerDeadline), "the two-minute helper deadline must not extend its caller's earlier deadline")
				} else {
					require.True(t, actual.After(started.Add(2*time.Minute-time.Second)) && actual.Before(started.Add(2*time.Minute+time.Second)), "helper request deadline=%s, want approximately two minutes after entry", actual)
				}
			case <-time.After(time.Second):
				t.Fatal("execWithStdin did not send an HTTP request carrying its bounded context")
			}
		})
	}
}
