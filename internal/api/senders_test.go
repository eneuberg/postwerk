package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/store"
)

// senderHarness has "app" in the senders file and, when managed, the
// database as the write side (POSTWERK_SENDER_KEYS=both).
type senderHarness struct {
	*harness
	db        *senders.DBSource
	reg       *senders.Registry
	fileKeyID string
}

func newSenderHarness(t *testing.T, managed bool) *senderHarness {
	t.Helper()
	sh := &senderHarness{}
	key, id := senders.NewKey()
	sh.fileKeyID = id
	line, _ := senders.KeyLine("app", key)
	path := filepath.Join(t.TempDir(), "senders")
	if err := os.WriteFile(path, []byte(line+"\napp:policy max_per_minute=60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := senders.NewFileSource(senders.FileOptions{Path: path, AllowEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	var st *store.Store
	sh.harness = newHarness(t, func(o *Options) {
		st = o.Events.(*store.Store)
		if managed {
			if sh.db, err = senders.NewDBSource(context.Background(), st, o.Now); err != nil {
				t.Fatal(err)
			}
			o.Senders = sh.db
		}
		if sh.reg, err = senders.NewRegistry(file, sh.db); err != nil {
			t.Fatal(err)
		}
		o.Query = query.New(query.Options{Store: st, Targets: []string{"mailjet"}, Now: o.Now, Senders: sh.reg})
	})
	return sh
}

var apiKeyRe = regexp.MustCompile(`pwk_[a-z2-7]{12}_[a-z2-7]{52}`)

func (h *harness) senderEvents() []string {
	h.t.Helper()
	var out []string
	for _, e := range h.events() {
		if strings.HasPrefix(e.Kind, "sender.") {
			out = append(out, e.Kind+" "+e.Sender+" "+e.Actor+" "+e.Text)
		}
	}
	return out
}

func TestSenderEndpointsAreForAdmins(t *testing.T) {
	sh := newSenderHarness(t, true)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/senders", ""},
		{"GET", "/v1/senders/app", ""},
		{"POST", "/v1/senders", `{"name":"shop"}`},
		{"POST", "/v1/senders/app/rotate", `{}`},
		{"POST", "/v1/senders/app/keys/" + sh.fileKeyID + "/revoke", `{}`},
		{"POST", "/v1/senders/app/revoke-all", `{}`},
		{"PUT", "/v1/senders/app/policy", `{}`},
	} {
		if r := sh.do(c.method, c.path, readBearer, c.body); r.StatusCode != 403 || !strings.Contains(r.body, `"forbidden"`) {
			t.Errorf("read %s %s: %d %s", c.method, c.path, r.StatusCode, r.body)
		}
		if r := sh.do(c.method, c.path, "", c.body); r.StatusCode != 401 {
			t.Errorf("no token %s %s: %d", c.method, c.path, r.StatusCode)
		}
	}
	forbidden := 0
	for _, e := range sh.apiEvents() {
		if strings.HasPrefix(e, "api.forbidden support sender") || e == "api.forbidden support senders" {
			forbidden++
		}
	}
	if forbidden != 7 {
		t.Fatalf("api.forbidden events: %q", sh.apiEvents())
	}
	if _, ok := sh.reg.ByName("shop"); ok || len(sh.senderEvents()) != 0 {
		t.Fatal("a read token changed something")
	}
	// The wrong method on the shared path.
	if r := sh.do("DELETE", "/v1/senders", adminBearer, ""); r.StatusCode != 405 || r.Header.Get("Allow") != "GET, POST" {
		t.Fatalf("DELETE /v1/senders: %d %q", r.StatusCode, r.Header.Get("Allow"))
	}
	if r := sh.do("DELETE", "/v1/senders/app", adminBearer, ""); r.StatusCode != 405 {
		t.Fatalf("DELETE a sender: %d", r.StatusCode)
	}
}

func TestSenderLifecycleByAPI(t *testing.T) {
	sh := newSenderHarness(t, true)
	// Create, with a policy and an expiry; the key comes back once.
	r := sh.do("POST", "/v1/senders", adminBearer,
		`{"name":"shop","policy":{"from":["*@shop.example.test"],"max_per_minute":30},"expires_at":"2027-01-01T00:00:00Z"}`)
	var iss issuedJSON
	r.json(t, &iss)
	if r.StatusCode != 201 || iss.Sender != "shop" || !apiKeyRe.MatchString(iss.Key) || iss.ExpiresAt == nil ||
		!iss.ExpiresAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("create: %d %s", r.StatusCode, r.body)
	}
	if id, _, _ := senders.ParseKey(iss.Key); id != iss.KeyID {
		t.Fatalf("key id %q, key %q", iss.KeyID, id)
	}
	if p, err := senders.NewAuthenticator(sh.reg, senders.AuthOptions{}).Authenticate("shop", iss.Key); err != nil || p.KeyID != iss.KeyID {
		t.Fatalf("the new key does not log in: %v", err)
	}
	_, secret, _ := senders.ParseKey(iss.Key)

	// Read it back: never the key.
	var s senderJSON
	r = sh.do("GET", "/v1/senders/shop", adminBearer, "")
	r.json(t, &s)
	if r.StatusCode != 200 || s.Source != "db" || s.ReadOnly || s.State != query.SenderActive || len(s.Keys) != 1 ||
		s.Keys[0].ID != iss.KeyID || s.Keys[0].State != query.KeyActive || s.Keys[0].CreatedBy != "support.admin" ||
		s.Policy.MaxPerMinute != 30 || len(s.Policy.From) != 1 || strings.Contains(r.body, secret) {
		t.Fatalf("sender: %d %s", r.StatusCode, r.body)
	}
	var list sendersResponse
	r = sh.do("GET", "/v1/senders", adminBearer, "")
	r.json(t, &list)
	if r.StatusCode != 200 || !list.Managed || len(list.Senders) != 2 || list.Senders[0].Name != "app" ||
		!list.Senders[0].ReadOnly || list.Senders[0].Source != "file" || list.Senders[1].Name != "shop" {
		t.Fatalf("list: %d %s", r.StatusCode, r.body)
	}

	// Rotate: a second key; a third is refused.
	var rot issuedJSON
	r = sh.do("POST", "/v1/senders/shop/rotate", adminBearer, `{}`)
	r.json(t, &rot)
	if r.StatusCode != 201 || rot.KeyID == iss.KeyID || !apiKeyRe.MatchString(rot.Key) || rot.ExpiresAt != nil {
		t.Fatalf("rotate: %d %s", r.StatusCode, r.body)
	}
	if r := sh.do("POST", "/v1/senders/shop/rotate", adminBearer, `{}`); r.StatusCode != 409 ||
		!strings.Contains(r.body, `"too_many_keys"`) || apiKeyRe.MatchString(r.body) {
		t.Fatalf("third key: %d %s", r.StatusCode, r.body)
	}
	// Revoke the old key, then once more.
	r = sh.do("POST", "/v1/senders/shop/keys/"+iss.KeyID+"/revoke", adminBearer, `{}`)
	s = senderJSON{}
	r.json(t, &s)
	if r.StatusCode != 200 || s.Keys[1].ID != iss.KeyID || s.Keys[1].State != query.KeyRevoked || s.Keys[1].RevokedBy != "support.admin" {
		t.Fatalf("revoke: %d %s", r.StatusCode, r.body)
	}
	if r := sh.do("POST", "/v1/senders/shop/keys/"+iss.KeyID+"/revoke", adminBearer, `{}`); r.StatusCode != 409 ||
		!strings.Contains(r.body, `"already_revoked"`) {
		t.Fatalf("revoke twice: %d %s", r.StatusCode, r.body)
	}
	// The policy is replaced as a whole.
	r = sh.do("PUT", "/v1/senders/shop/policy", adminBearer, `{"from":["<>","*@shop.example.test"],"max_rcpt_per_hour":500}`)
	s = senderJSON{}
	r.json(t, &s)
	if r.StatusCode != 200 || s.Policy.MaxPerMinute != 0 || s.Policy.MaxRcptPerHour != 500 || len(s.Policy.From) != 2 {
		t.Fatalf("policy: %d %s", r.StatusCode, r.body)
	}
	// Revoke all: the sender is switched off and stays.
	var all revokeAllResponse
	r = sh.do("POST", "/v1/senders/shop/revoke-all", adminBearer, `{}`)
	r.json(t, &all)
	if r.StatusCode != 200 || len(all.Revoked) != 1 || all.Revoked[0] != rot.KeyID || all.Sender.State != query.SenderOff {
		t.Fatalf("revoke all: %d %s", r.StatusCode, r.body)
	}
	if r := sh.do("POST", "/v1/senders/shop/revoke-all", adminBearer, `{}`); r.StatusCode != 409 ||
		!strings.Contains(r.body, `"nothing_to_revoke"`) {
		t.Fatalf("revoke all twice: %d %s", r.StatusCode, r.body)
	}

	want := []string{
		"sender.created shop support.admin key=" + iss.KeyID,
		"sender.rotated shop support.admin key=" + rot.KeyID,
		"sender.revoked shop support.admin key=" + iss.KeyID,
		"sender.policy_changed shop support.admin from_patterns=2 max_per_minute=0 max_rcpt_per_hour=500",
		"sender.revoked shop support.admin key=" + rot.KeyID,
	}
	if got := sh.senderEvents(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A change writes its sender.* event instead of api.request; reads and
	// refusals are api.request.
	reqs := strings.Join(sh.apiEvents(), "\n")
	for _, want := range []string{"api.request support.admin sender status=200 results=1",
		"api.request support.admin senders status=200 results=2",
		"api.request support.admin sender_rotate status=409 results=0",
		"api.request support.admin sender_revoke status=409 results=0",
		"api.request support.admin sender_revoke_all status=409 results=0"} {
		if !strings.Contains(reqs, want) {
			t.Errorf("no %q in\n%s", want, reqs)
		}
	}
	if strings.Contains(reqs, "sender_create") || strings.Contains(reqs, "sender_policy") {
		t.Errorf("a recorded change also wrote api.request:\n%s", reqs)
	}
	// Neither key is in an event or the log.
	for _, k := range []string{iss.Key, rot.Key} {
		_, sec, _ := senders.ParseKey(k)
		if strings.Contains(sh.logs.String(), sec) {
			t.Fatal("a key is in the log")
		}
		for _, e := range sh.events() {
			if strings.Contains(fmt.Sprintf("%+v", e), sec) {
				t.Fatalf("a key is in event %s", e.Kind)
			}
		}
	}
}

func TestSenderErrorsByAPI(t *testing.T) {
	sh := newSenderHarness(t, true)
	if _, err := sh.db.Create(context.Background(), senders.CreateRequest{Name: "shop", Actor: "ops"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", "/v1/senders", `{"name":"Shop"}`, 400, "invalid_name"},
		{"POST", "/v1/senders", `{"name":""}`, 400, "invalid_name"},
		{"POST", "/v1/senders", `{"name":"x","policy":{"from":["nope"]}}`, 400, "invalid_policy"},
		{"POST", "/v1/senders", `{"name":"x","policy":{"max_per_minute":-1}}`, 400, "invalid_policy"},
		{"POST", "/v1/senders", `{"name":"x","expires_at":"2026-11-15T11:00:00Z"}`, 400, "invalid_expiry"},
		{"POST", "/v1/senders", `{"name":"x","expires_at":"tomorrow"}`, 400, "bad_request"},
		{"POST", "/v1/senders", `{"name":"x","key":"pwk_mine"}`, 400, "bad_request"},
		{"POST", "/v1/senders", `{"name":"shop"}`, 409, "name_taken"},
		{"POST", "/v1/senders", `{"name":"app"}`, 409, "name_taken"},
		{"POST", "/v1/senders/nobody/rotate", `{}`, 404, "not_found"},
		{"POST", "/v1/senders/Bad/rotate", `{}`, 404, "not_found"},
		{"POST", "/v1/senders/shop/rotate", ``, 415, "unsupported_media_type"},
		{"POST", "/v1/senders/shop/rotate", `{"expires_at":"2026-01-01T00:00:00Z"}`, 400, "invalid_expiry"},
		{"POST", "/v1/senders/shop/keys/aaaaaaaaaaaa/revoke", `{}`, 404, "not_found"},
		{"POST", "/v1/senders/shop/keys/" + sh.fileKeyID + "/revoke", `{}`, 404, "not_found"},
		{"PUT", "/v1/senders/shop/policy", `{"from":["a@b@c"]}`, 400, "invalid_policy"},
		{"PUT", "/v1/senders/shop/policy", `{"from":"*@x"}`, 400, "bad_request"},
		{"POST", "/v1/senders/shop/policy", `{}`, 405, "method_not_allowed"},
		{"GET", "/v1/senders?x=1", "", 400, "bad_request"},
		// The file sender is read-only.
		{"POST", "/v1/senders/app/rotate", `{}`, 409, "file_sender"},
		{"POST", "/v1/senders/app/keys/" + sh.fileKeyID + "/revoke", `{}`, 409, "file_sender"},
		{"POST", "/v1/senders/app/revoke-all", `{}`, 409, "file_sender"},
		{"PUT", "/v1/senders/app/policy", `{"max_per_minute":1}`, 409, "file_sender"},
	} {
		r := sh.do(c.method, c.path, adminBearer, c.body)
		if r.StatusCode != c.status || !strings.Contains(r.body, `"error":"`+c.code+`"`) {
			t.Errorf("%s %s %s: %d %s, want %d %s", c.method, c.path, c.body, r.StatusCode, r.body, c.status, c.code)
		}
	}
	app, _ := sh.reg.ByName("app")
	if app.Policy.MaxPerMinute() != 60 || !app.Keys[0].RevokedAt.IsZero() {
		t.Fatal("the file sender changed")
	}
	if got := sh.senderEvents(); len(got) != 1 {
		t.Fatalf("refusals wrote %q", got)
	}

	// File only: every change is refused, reading works.
	fh := newSenderHarness(t, false)
	if r := fh.do("POST", "/v1/senders", adminBearer, `{"name":"shop"}`); r.StatusCode != 409 || !strings.Contains(r.body, `"db_off"`) {
		t.Fatalf("create in file mode: %d %s", r.StatusCode, r.body)
	}
	if r := fh.do("POST", "/v1/senders/app/rotate", adminBearer, `{}`); r.StatusCode != 409 || !strings.Contains(r.body, `"db_off"`) {
		t.Fatalf("rotate in file mode: %d %s", r.StatusCode, r.body)
	}
	var list sendersResponse
	r := fh.do("GET", "/v1/senders", adminBearer, "")
	r.json(t, &list)
	if r.StatusCode != 200 || list.Managed || len(list.Senders) != 1 || !list.Senders[0].ReadOnly {
		t.Fatalf("list in file mode: %s", r.body)
	}
}

func TestMailsBySenderAndAuditByAPI(t *testing.T) {
	sh := newSenderHarness(t, true)
	ctx := context.Background()
	now := sh.clock.Now()
	for i, m := range []struct{ sender, key string }{{"app", sh.fileKeyID}, {"app", ""}, {"other", ""}} {
		if err := sh.st.Accept(ctx, store.NewMail{ID: fmt.Sprintf("pw_A%d", i), ReceivedAt: now.Add(-time.Duration(i) * time.Minute),
			Sender: m.sender, Recipients: []string{"r@example.test"}, Raw: []byte{0},
			Deliveries: []store.NewDelivery{{Target: "mailjet", Status: store.Queued}},
			Audit: store.MailAudit{KeyID: m.key, KeySource: "file", ClientIP: "10.0.0.9", TLSVersion: "TLS 1.3",
				TLSCipher: "TLS_AES_128_GCM_SHA256", EHLO: "app-1", AuthMech: "PLAIN"}}); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string]string{
		"sender=app":                          "pw_A0,pw_A1",
		"key_id=" + sh.fileKeyID:              "pw_A0",
		"sender=other&key_id=" + sh.fileKeyID: "",
	} {
		var l listResponse
		r := sh.do("GET", "/v1/mails?"+q, adminBearer, "")
		r.json(t, &l)
		if r.StatusCode != 200 || strings.Join(ids(l.Mails), ",") != want {
			t.Errorf("%s: %d %s", q, r.StatusCode, r.body)
		}
	}
	for _, bad := range []string{"sender=Bad%20Name", "key_id=short", "key_id=a&key_id=b"} {
		if r := sh.do("GET", "/v1/mails?"+bad, adminBearer, ""); r.StatusCode != 400 {
			t.Errorf("%s: %d", bad, r.StatusCode)
		}
	}
	var d mailDetail
	r := sh.do("GET", "/v1/mails/pw_A0", adminBearer, "")
	r.json(t, &d)
	if d.Audit == nil || d.Audit.KeyID != sh.fileKeyID || d.Audit.KeySource != "file" || d.Audit.ClientIP != "10.0.0.9" ||
		d.Audit.TLSVersion != "TLS 1.3" || d.Audit.EHLO != "app-1" || d.Audit.AuthMech != "PLAIN" {
		t.Fatalf("admin audit: %s", r.body)
	}
	// A read token gets no audit block (mail.accepted names the key and
	// the client in its text for both, D65).
	r = sh.do("GET", "/v1/mails/pw_A0", readBearer, "")
	if r.StatusCode != 200 || strings.Contains(r.body, `"audit"`) || strings.Contains(r.body, "TLS_AES_128_GCM_SHA256") {
		t.Fatalf("read sees the audit: %s", r.body)
	}
}
