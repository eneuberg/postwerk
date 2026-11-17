package delivery

import (
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

// gate sits in front of a fake target. While closed it accepts a
// connection and closes it at once, so the greeting fails (unreachable);
// while open it passes the connection through.
type gate struct {
	addr  string
	open  atomic.Bool
	conns atomic.Int32 // connections while closed
}

func newGate(t *testing.T, to string) *gate {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &gate{addr: l.Addr().String()}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if !g.open.Load() {
				g.conns.Add(1)
				c.Close()
				continue
			}
			go pipe(c, to)
		}
	}()
	return g
}

func pipe(c net.Conn, to string) {
	defer c.Close()
	u, err := net.Dial("tcp", to)
	if err != nil {
		return
	}
	defer u.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(u, c); u.(*net.TCPConn).CloseWrite() }()
	go func() { defer wg.Done(); io.Copy(c, u); c.(*net.TCPConn).CloseWrite() }()
	wg.Wait()
}

func gated(name string, g *gate) config.Target {
	host, _, _ := net.SplitHostPort(g.addr)
	return config.Target{Name: name, Scheme: config.SchemePlain, Host: host, Addr: g.addr,
		RetryWindow: time.Hour, OnUnknown: config.Hold}
}

// An unreachable target that is back by the first probe writes neither
// target.down nor target.recovered (D58); the mail goes out.
func TestUnreachableBlipWritesNothing(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	g := newGate(t, f.addr)
	h := startPostwerk(t, []config.Target{gated("blip", g)},
		harnessOpts{downAfterProbes: 1, probeInterval: 300 * time.Millisecond})

	id := h.send([]string{canary}, testMsg)
	testutil.Eventually(t, timeout, "first attempt fails", func() bool { return g.conns.Load() >= 1 })
	g.open.Store(true)
	h.waitStatus(id, "blip", store.HandedOff)
	if n := len(h.targetEvents("blip", store.KindTargetDown)); n != 0 {
		t.Fatalf("%d target.down for a blip", n)
	}
	if n := len(h.targetEvents("blip", store.KindTargetRecovered)); n != 0 {
		t.Fatalf("%d target.recovered for a blip", n)
	}
}

// An unreachable target is reported once its first probe has failed too,
// not at the failed attempt; target.recovered follows when it is back.
func TestUnreachableReportedAfterProbe(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	g := newGate(t, f.addr)
	h := startPostwerk(t, []config.Target{gated("flaky", g)},
		harnessOpts{downAfterProbes: 1, probeInterval: 300 * time.Millisecond})

	id := h.send([]string{canary}, testMsg)
	testutil.Eventually(t, timeout, "first attempt fails", func() bool { return g.conns.Load() >= 1 })
	time.Sleep(100 * time.Millisecond) // well before the first probe
	if n := len(h.targetEvents("flaky", store.KindTargetDown)); n != 0 {
		t.Fatalf("target.down before any probe failed (%d)", n)
	}
	testutil.Eventually(t, timeout, "target down after a failed probe", func() bool {
		return len(h.targetEvents("flaky", store.KindTargetDown)) == 1
	})
	if g.conns.Load() < 2 {
		t.Fatalf("target.down after %d connections, want the attempt and a probe", g.conns.Load())
	}
	down := h.targetEvents("flaky", store.KindTargetDown)[0]
	if down.Level != store.Error || !strings.HasPrefix(down.Text, "unreachable: ") {
		t.Fatalf("target.down = %+v", down)
	}
	g.open.Store(true)
	h.waitStatus(id, "flaky", store.HandedOff)
	testutil.Eventually(t, timeout, "target recovered", func() bool {
		return len(h.targetEvents("flaky", store.KindTargetRecovered)) == 1
	})
	if n := len(h.targetEvents("flaky", store.KindTargetDown)); n != 1 {
		t.Fatalf("%d target.down, want 1 per outage", n)
	}
}

// A rejected login is reported at once, whatever DownAfterProbes says.
func TestAuthDownIsImmediate(t *testing.T) {
	cert := testutil.NewCert(t)
	mj := newTarget(t, startTLS, &cert, "apikey", "apisecret")
	mj.set(func(b *behavior) { b.authFail = true })
	h := startPostwerk(t, []config.Target{target("mailjet", mj, startTLS)},
		harnessOpts{rootCAs: &cert, downAfterProbes: 3, probeInterval: time.Hour})

	h.send([]string{canary}, testMsg)
	testutil.Eventually(t, timeout, "target down", func() bool {
		return len(h.targetEvents("mailjet", store.KindTargetDown)) == 1
	})
	if d := h.targetEvents("mailjet", store.KindTargetDown)[0]; d.Code != 535 || !strings.HasPrefix(d.Text, "auth") {
		t.Fatalf("target.down = %+v", d)
	}
}
