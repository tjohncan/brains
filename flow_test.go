package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// newStore is a fresh database with the two roles the audit loop uses.
func newStore(t *testing.T) *store {
	t.Helper()
	db, _ := newDB(t)
	st := &store{db: db}
	mustDefine(t, st, "fixer", "You fix what's filed against your repo.", false)
	mustDefine(t, st, "auditor", "You read the repo cold and file what you find.", true)
	return st
}

func mustDefine(t *testing.T, st *store, name, description string, freshEyes bool) {
	t.Helper()
	if _, err := st.defineRole(name, description, &freshEyes); err != nil {
		t.Fatalf("define %s: %v", name, err)
	}
}

func mustRegister(t *testing.T, st *store, role string) *sessionInfo {
	t.Helper()
	sess, err := st.register(registration{role: role, repo: "owner/app"})
	if err != nil {
		t.Fatalf("register %s: %v", role, err)
	}
	return sess
}

// statements is what the beliefs say, without their scopes.
func statements(beliefs []scopedBelief) []string {
	var out []string
	for _, b := range beliefs {
		out = append(out, b.statement)
	}
	return out
}

func mustFile(t *testing.T, st *store, from *sessionInfo, in fileInput) string {
	t.Helper()
	code, err := st.file(from, in)
	if err != nil {
		t.Fatalf("file %q: %v", in.title, err)
	}
	return code
}

// TestAuditLoop runs one auditor's findings through two fixers: filing, the
// inbox, a claim race, putting a task down, outcomes and resolutions, and the
// auditor reading what happened.
func TestAuditLoop(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	rival := mustRegister(t, st, "fixer")
	auditor := mustRegister(t, st, "auditor")
	finding := func(title, severity string) fileInput {
		return fileInput{typ: "audit_finding", title: title, body: "details", severity: severity,
			toRole: "fixer", repo: "owner/app"}
	}
	low := mustFile(t, st, auditor, finding("Stale comment in db_init.c", "low"))
	high := mustFile(t, st, auditor, finding("Refresh token reuse not detected", "high"))

	if _, err := st.inbox(fixer, ""); err == nil {
		t.Error("an inbox read without a purpose was allowed")
	}
	rows, err := st.inbox(fixer, "start of the fix pass")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].code != high || rows[1].code != low {
		t.Fatalf("fixer inbox = %v, want %s then %s", rows, high, low)
	}
	if rows, err := st.inbox(auditor, "anything for me"); err != nil || len(rows) != 0 {
		t.Errorf("auditor inbox = %v, %v; want empty", rows, err)
	}

	if err := st.claim(fixer, high); err != nil {
		t.Fatal(err)
	}
	if err := st.claim(rival, high); err == nil || !strings.Contains(err.Error(), "already claimed by "+fixer.code) {
		t.Errorf("second claim: err = %v, want already claimed by %s", err, fixer.code)
	}
	if err := st.close(fixer, high, "fixed", "", ""); err == nil {
		t.Error("fixed without a resolution was accepted")
	}
	if err := st.close(fixer, high, "fixed", "", "Detected reuse and revoked the chain; see commit abc123."); err != nil {
		t.Fatal(err)
	}

	// Putting a task down: only its claimer can, and the note goes with it.
	if err := st.claim(rival, low); err != nil {
		t.Fatal(err)
	}
	if err := st.release(fixer, low, "not mine to release"); err == nil {
		t.Error("a session released another session's claim")
	}
	if err := st.release(rival, low, "Same root cause as the reuse finding, I think."); err != nil {
		t.Fatal(err)
	}
	d, err := st.show(fixer, low, "pick up where the other fixer stopped", false)
	if err != nil {
		t.Fatal(err)
	}
	if d.status != "open" || !strings.Contains(d.note, "root cause") {
		t.Errorf("released task: status %q, note %q; want open, with the note", d.status, d.note)
	}

	if err := st.close(fixer, low, "duplicate", "", ""); err == nil {
		t.Error("a duplicate without duplicate_of was accepted")
	}
	if err := st.close(fixer, low, "duplicate", high, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.close(fixer, low, "fixed", "", "again"); err == nil {
		t.Error("closing a finished task again was accepted")
	}

	mine, err := st.mine(auditor, "what became of my findings")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range mine {
		got[r.code] = statusText(r)
	}
	if want := map[string]string{high: "done: fixed", low: "done: duplicate of " + high}; !reflect.DeepEqual(got, want) {
		t.Errorf("auditor sees %v, want %v", got, want)
	}
	d, err = st.show(auditor, high, "read the fixer's resolution", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.resolution, "abc123") {
		t.Errorf("resolution = %q, want the fixer's words", d.resolution)
	}

	for _, in := range []fileInput{{toSession: auditor.nickname}, {toRole: "auditor", repo: "owner/app"}} {
		in.typ, in.title = "handoff", "Please re-audit"
		if _, err := st.file(fixer, in); err == nil || !strings.Contains(err.Error(), "fresh eyes") {
			t.Errorf("a handoff to fresh eyes (%+v): err = %v, want refused: nothing there reads it", in, err)
		}
	}
	handoff := mustFile(t, st, rival, fileInput{typ: "handoff", title: "Take over CORS", body: "notes", toSession: fixer.code})
	if _, err := st.show(auditor, handoff, "peek", true); err == nil {
		t.Error("fresh eyes opened a task someone else filed")
	}

	var unexplained int
	if err := st.db.QueryRow("select count(*) from action where session_pin is not null and purpose is null").Scan(&unexplained); err != nil {
		t.Fatal(err)
	}
	if unexplained != 0 {
		t.Errorf("%d session actions have no purpose", unexplained)
	}
}

// TestDuplicatesNameTheOriginal: a duplicate of a duplicate names the task
// that carries the outcome, and a loop is refused.
func TestDuplicatesNameTheOriginal(t *testing.T) {
	st := newStore(t)
	fixer, auditor := mustRegister(t, st, "fixer"), mustRegister(t, st, "auditor")
	file := func(title string) string {
		return mustFile(t, st, auditor, fileInput{title: title, body: "b", toRole: "fixer", repo: "owner/app"})
	}
	original, repeat, again := file("Original"), file("Repeat"), file("Again")
	if err := st.close(fixer, repeat, "duplicate", original, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.close(fixer, again, "duplicate", repeat, ""); err != nil {
		t.Fatal(err)
	}
	if d, err := st.show(nil, again, "", false); err != nil || d.dupOf != original {
		t.Errorf("a duplicate of a duplicate names %v, %v; want %s", d, err, original)
	}
	if err := st.close(fixer, original, "duplicate", again, ""); err == nil {
		t.Error("closed the original as a duplicate of its own duplicate")
	}
}

func TestResumeKeepsTheSession(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	again, err := st.resume(fixer.code, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.pin != fixer.pin || again.role != "fixer" {
		t.Errorf("resumed as %+v, want %+v", again, fixer)
	}
	if _, err := st.resume("20260101-nobody-xxxx", ""); err == nil {
		t.Error("resuming an unknown session succeeded")
	}

	// A session belongs to its conversation; one with none recorded takes the
	// conversation that resumes it.
	other, err := st.register(registration{role: "fixer", repo: "owner/app", externalID: "conversation-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.resume(other.code, "conversation-b"); err == nil || !strings.Contains(err.Error(), "another conversation") {
		t.Errorf("resume from another conversation: err = %v, want refused", err)
	}
	if _, err := st.resume(other.code, "conversation-a"); err != nil {
		t.Errorf("resume in its own conversation: %v", err)
	}
	if again, err := st.resume(fixer.code, "conversation-c"); err != nil || again.externalID != "conversation-c" {
		t.Errorf("resume of a session with no conversation = %+v, %v; want conversation-c recorded", again, err)
	}
	if sess, _, err := sessionByConversation(st.db, "conversation-c"); err != nil || sess == nil || sess.pin != fixer.pin {
		t.Errorf("sessionByConversation(conversation-c) = %+v, %v; want %s", sess, err, fixer.code)
	}
}

// mcpExchange feeds requests to the MCP server and returns its replies by id.
func mcpExchange(t *testing.T, st *store, session string, requests ...string) map[string]map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := serveMCP(st, session, strings.NewReader(strings.Join(requests, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	replies := map[string]map[string]any{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		replies[fmt.Sprint(r["id"])] = r
	}
	return replies
}

func toolText(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", reply)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in %v", res)
	}
	return content[0].(map[string]any)["text"].(string), res["isError"] == true
}

func toolNames(t *testing.T, reply map[string]any) []string {
	t.Helper()
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", reply)
	}
	var names []string
	for _, tool := range res["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

// toolCall is a tools/call request.
func toolCall(id int, name string, args map[string]string) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	return string(b)
}

const initializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

// TestMCPFreshEyes is an auditor brains fresh registered: it's told who it
// is and what holds, and it can only file and follow up.
func TestMCPFreshEyes(t *testing.T) {
	st := newStore(t)
	if _, err := st.believe(nil, "Never push; the owner pushes.", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	auditor := mustRegister(t, st, "auditor")
	r := mcpExchange(t, st, auditor.code,
		initializeRequest,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		toolCall(3, "register", map[string]string{"role": "fixer", "repo": "o/r"}),
		toolCall(4, "inbox", map[string]string{"purpose": "peek"}),
		toolCall(5, "file", map[string]string{"title": "t", "body": "b", "to_role": "fixer", "repo": "o/r"}),
	)
	if len(r) != 5 {
		t.Errorf("%d replies, want 5: the notification gets none", len(r))
	}
	hello, ok := r["1"]["result"].(map[string]any)
	if !ok || hello["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %v, want protocolVersion 2025-06-18 echoed", r["1"])
	}
	unknown := mcpExchange(t, st, auditor.code, strings.Replace(initializeRequest, "2025-06-18", "1999-01-01", 1))
	if got := unknown["1"]["result"].(map[string]any)["protocolVersion"]; got != protocolVersions[len(protocolVersions)-1] {
		t.Errorf("initialize asking for 1999-01-01 = %v, want the newest version brains is written to", got)
	}
	told, _ := hello["instructions"].(string)
	for _, want := range []string{auditor.code + " (auditor1): auditor on owner/app", "You read the repo cold", "fresh eyes", "- Never push"} {
		if !strings.Contains(told, want) {
			t.Errorf("instructions lack %q:\n%s", want, told)
		}
	}
	if got, want := toolNames(t, r["2"]), []string{"file", "withdraw", "mine", "show", "brief"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fresh tools = %v, want %v", got, want)
	}
	if text, isErr := toolText(t, r["3"]); !isErr || !strings.Contains(text, "already knows this session as "+auditor.code) {
		t.Errorf("register = %q, %v; want refused, naming the session", text, isErr)
	}
	if text, isErr := toolText(t, r["4"]); !isErr || !strings.Contains(text, "fresh eyes") {
		t.Errorf("inbox with fresh eyes = %q, %v; want refused", text, isErr)
	}
	if text, isErr := toolText(t, r["5"]); isErr || !strings.HasPrefix(text, "Filed ") {
		t.Errorf("file = %q, %v; want filed", text, isErr)
	}
}

// TestMCPFullMode is a plain Claude Code session: it registers itself, its
// role in any case, and gets the full tools.
func TestMCPFullMode(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "") // no conversation for brains to recognize
	st := newStore(t)
	for _, b := range [][2]string{{"Never push; the owner pushes.", ""}, {"No diary comments.", "fixer"}} {
		if _, err := st.believe(nil, b[0], b[1], "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	r := mcpExchange(t, st, "",
		initializeRequest,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		toolCall(3, "inbox", map[string]string{"purpose": "too soon"}),
		toolCall(4, "register", map[string]string{"role": "fixr", "repo": "o/r"}),
		toolCall(5, "register", map[string]string{"role": "Fixer", "repo": "o/r"}),
		toolCall(6, "inbox", map[string]string{}),
		toolCall(7, "inbox", map[string]string{"purpose": "first look"}),
		`{"jsonrpc":"2.0","id":8,"method":"no/such/method"}`,
	)
	told, _ := r["1"]["result"].(map[string]any)["instructions"].(string)
	if !strings.Contains(told, "Call register once") || !strings.Contains(told, "- Never push") || strings.Contains(told, "diary") {
		t.Errorf("instructions before register = %q, want register asked for and only the beliefs for every session", told)
	}
	want := []string{"register", "file", "withdraw", "inbox", "mine", "show", "claim", "release", "close", "list", "propose", "brief"}
	if got := toolNames(t, r["2"]); !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	if text, isErr := toolText(t, r["3"]); !isErr || !strings.Contains(text, "register first") {
		t.Errorf("inbox before register = %q, %v; want refused", text, isErr)
	}
	if text, isErr := toolText(t, r["4"]); !isErr || !strings.Contains(text, "the roles are auditor, fixer") {
		t.Errorf("register in a role nobody defined = %q, %v; want refused, naming the roles", text, isErr)
	}
	text, isErr := toolText(t, r["5"])
	if isErr || !strings.Contains(text, ": fixer on o/r.") || !strings.Contains(text, "You fix what's filed") ||
		!strings.Contains(text, "- No diary comments.") || !strings.Contains(text, "register with resume") {
		t.Errorf("register = %q, %v; want the role, lowercased, what it's for, its beliefs, and how to resume "+
			"after a restart, since there's no conversation to recognize", text, isErr)
	}
	if text, isErr := toolText(t, r["6"]); !isErr || !strings.Contains(text, "purpose is required") {
		t.Errorf("inbox without purpose = %q, %v; want refused", text, isErr)
	}
	if text, isErr := toolText(t, r["7"]); isErr || text != "No tasks.\n" {
		t.Errorf("inbox = %q, %v; want no tasks", text, isErr)
	}
	if r["8"]["error"] == nil {
		t.Errorf("unknown method = %v, want a JSON-RPC error", r["8"])
	}
}

// TestRegisteringIntoFreshEyes is a plain session registering in a fresh-eyes
// role: the full tools it was shown stop working, and stay stopped.
func TestRegisteringIntoFreshEyes(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	st := newStore(t)
	r := mcpExchange(t, st, "",
		toolCall(1, "register", map[string]string{"role": "auditor", "repo": "o/r"}),
		toolCall(2, "inbox", map[string]string{"purpose": "peek"}),
		toolCall(3, "register", map[string]string{"role": "fixer", "repo": "o/r"}),
		toolCall(4, "list", map[string]string{"purpose": "peek again"}),
		`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`,
	)
	if note := r["<nil>"]; note == nil || note["method"] != "notifications/tools/list_changed" {
		t.Errorf("after register, notification = %v; want tools/list_changed", note)
	}
	if got, want := toolNames(t, r["5"]), []string{"file", "withdraw", "mine", "show", "brief"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools after registering into fresh eyes = %v, want %v", got, want)
	}
	if text, _ := toolText(t, r["1"]); !strings.Contains(text, "fresh eyes") {
		t.Errorf("register = %q, want it told about fresh eyes", text)
	}
	for _, id := range []string{"2", "4"} {
		if text, isErr := toolText(t, r[id]); !isErr || !strings.Contains(text, "fresh eyes") {
			t.Errorf("reply %s = %q, %v; want refused for fresh eyes", id, text, isErr)
		}
	}
	if text, isErr := toolText(t, r["3"]); !isErr || !strings.Contains(text, "already knows this session") {
		t.Errorf("a second register = %q, %v; want refused: one server serves one session", text, isErr)
	}
	if n := one(t, st.db, "select count(*) from session"); n != "1" {
		t.Errorf("%s sessions recorded, want 1", n)
	}
}

// TestFreshFixer is a fixer brains fresh registered: it never registers,
// and it's told every belief that reaches it, broadest first.
func TestFreshFixer(t *testing.T) {
	st := newStore(t)
	for _, b := range [][3]string{
		{"Never push; the owner pushes.", "", ""},
		{"No diary comments.", "fixer", ""},
		{"Build in WSL, against a copy.", "", "owner/app"},
		{"Auditors only.", "auditor", ""},
		{"Another repo's rule.", "", "owner/other"},
	} {
		if _, err := st.believe(nil, b[0], b[1], b[2], "", ""); err != nil {
			t.Fatal(err)
		}
	}
	fixer, err := st.register(registration{role: "fixer", repo: "owner/app", externalID: uuidString(newUUID())})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixer.externalID) != 36 || one(t, st.db, "select external_id from session where pin = ?", fixer.pin) != fixer.externalID {
		t.Errorf("external_id = %q, want the conversation's UUID recorded", fixer.externalID)
	}
	r := mcpExchange(t, st, fixer.code[len(fixer.code)-4:], initializeRequest, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		toolCall(3, "brief", nil))
	told, _ := r["1"]["result"].(map[string]any)["instructions"].(string)
	want := "For every session:\n- Never push; the owner pushes.\n\nOn owner/app:\n" +
		"- Build in WSL, against a copy.\n\nFor every fixer:\n- No diary comments."
	if !strings.Contains(told, fixer.called()+": fixer on owner/app") || !strings.Contains(told, "Check the handoff") ||
		!strings.Contains(told, want) || strings.Contains(told, "Auditors only") || strings.Contains(told, "Another repo") {
		t.Errorf("instructions = %q\nwant the session, how to work, and exactly the beliefs that reach it:\n%s", told, want)
	}
	want2 := []string{"file", "withdraw", "inbox", "mine", "show", "claim", "release", "close", "list", "propose", "brief"}
	if got := toolNames(t, r["2"]); !reflect.DeepEqual(got, want2) {
		t.Errorf("tools = %v, want %v", got, want2)
	}
	if text, isErr := toolText(t, r["3"]); isErr || !strings.Contains(text, want) || !strings.Contains(text, "Check the handoff") {
		t.Errorf("brief = %q, %v; want the briefing again, beliefs included", text, isErr)
	}
}

func TestImprovementsCloseWithoutAnOutcome(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	idea := mustFile(t, st, fixer, fileInput{typ: "improvement", title: "Nightly fuzzing",
		body: "Run the fuzzer on a schedule.", toRole: "fixer", repo: "owner/app"})
	if !strings.Contains(idea, "-improvement-") {
		t.Errorf("code = %s, want the type in it", idea)
	}
	_, err := st.file(fixer, fileInput{typ: "wish", title: "x", toRole: "fixer", repo: "owner/app"})
	if err == nil || !strings.Contains(err.Error(), "improvement") {
		t.Errorf("unknown type: err = %v, want the known types listed", err)
	}
	rows, err := st.list(fixer, "improvements only", "all", "", "improvement", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].code != idea || rows[0].typ != "improvement" {
		t.Errorf("list by type = %v, want just %s", rows, idea)
	}
	if _, err := st.list(fixer, "findings, by the word in their codes", "all", "", "finding", false); err == nil {
		t.Error("listed tasks of a type that doesn't exist")
	}
	if err := st.close(fixer, idea, "", "", ""); err != nil {
		t.Errorf("closing an improvement without an outcome: %v", err)
	}
}

// TestWithdrawAndTriageMarker covers a filer taking back its own duplicate,
// and lists telling a confirmed-and-released finding from an untouched one.
func TestWithdrawAndTriageMarker(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	auditor := mustRegister(t, st, "auditor")
	file := func(title string) string {
		return mustFile(t, st, auditor, fileInput{title: title, body: "b", toRole: "fixer", repo: "owner/app"})
	}
	first, again, confirmed := file("Introspect ignores deactivation"), file("Introspect keeps ignoring deactivation"), file("Something else")

	if err := st.withdraw(auditor, again, ""); err == nil {
		t.Error("a withdrawal without a reason was accepted")
	}
	if err := st.withdraw(fixer, again, "not mine"); err == nil {
		t.Error("a session withdrew someone else's filing")
	}
	if err := st.withdraw(auditor, again, "Duplicate of my own "+first+"; the first call landed after all."); err != nil {
		t.Fatal(err)
	}
	if err := st.claim(fixer, confirmed); err != nil {
		t.Fatal(err)
	}
	if err := st.withdraw(auditor, confirmed, "changed my mind"); err == nil {
		t.Error("a claimed task was withdrawn out from under its claimer")
	}
	if err := st.release(fixer, confirmed, "Confirmed; the fix is one predicate."); err != nil {
		t.Fatal(err)
	}

	rows, err := st.list(fixer, "triage state", "all", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.code] = statusText(r)
	}
	if want := map[string]string{first: "open", again: "dropped", confirmed: "open, noted"}; !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

// TestCodesByTheirEnding: commands take the four characters agents cite when
// no other code ends that way, and name the candidates when one does.
func TestCodesByTheirEnding(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	code := mustFile(t, st, fixer, fileInput{title: "One", body: "b", toRole: "fixer", repo: "owner/app"})
	ending := code[len(code)-4:]
	for _, typed := range []string{code, ending, "finding-" + ending, " " + ending + " "} {
		if d, err := st.show(fixer, typed, "by its ending", false); err != nil || d.code != code {
			t.Errorf("show %q = %v, %v; want %s", typed, d, err, code)
		}
	}
	if _, err := st.show(fixer, ending[1:], "part of an ending", false); err == nil {
		t.Errorf("show %q found a task; an ending starts after a hyphen", ending[1:])
	}

	twin := "20250101-finding-" + ending
	mustExec(t, st.db, `insert into task (id, code_name, task_type_pin, title, assignee_role_pin, repo_pin)
		select randomblob(16), ?, task_type_pin, 'Twin', assignee_role_pin, repo_pin from task where code_name = ?`, twin, code)
	if _, err := st.show(fixer, ending, "an ending two codes share", false); err == nil ||
		!strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), twin) {
		t.Errorf("shared ending: err = %v, want both codes named", err)
	}
	if err := st.claim(fixer, code); err != nil {
		t.Fatal(err)
	}
	if err := st.claim(fixer, code); err == nil || !strings.HasPrefix(err.Error(), code+" is already claimed") {
		t.Errorf("second claim: err = %v, want it to name the full code", err)
	}

	handoff := mustFile(t, st, fixer, fileInput{typ: "handoff", title: "To myself", toSession: fixer.code[len(fixer.code)-4:]})
	if d, err := st.show(fixer, handoff, "who it's for", false); err != nil || d.to != fixer.code {
		t.Errorf("handoff to a session by its ending: to = %v, %v; want %s", d, err, fixer.code)
	}
}

// TestRolesAreDefinedNotTyped: a role exists before anyone registers in it or
// files to it, and names match in any case.
func TestRolesAreDefinedNotTyped(t *testing.T) {
	db, _ := newDB(t)
	st := &store{db: db}
	if _, err := st.register(registration{role: "fixer", repo: "o/r"}); err == nil || !strings.Contains(err.Error(), "no roles are defined") {
		t.Errorf("register with no roles: err = %v, want refused", err)
	}
	for _, bad := range []string{"Bad Name", "fixer!", "-fixer", "2fa", "a-very-long-role-name-indeed"} {
		if _, err := st.defineRole(bad, "x", nil); err == nil {
			t.Errorf("defined a role named %q", bad)
		}
	}
	if _, err := st.defineRole("fixer", "", nil); err == nil {
		t.Error("defined a role without a description")
	}
	mustDefine(t, st, "fixer", "You fix.", false)
	if _, err := st.defineRole("fixer", "", nil); err == nil {
		t.Error("a change with nothing in it was accepted")
	}
	yes := true
	if created, err := st.defineRole("FIXER", "", &yes); err != nil || created {
		t.Errorf("change = %v, %v; want changed, not created", created, err)
	}
	if r, err := findRole(db, "fixer"); err != nil || !r.freshEyes || r.description != "You fix." {
		t.Errorf("changed role = %+v, %v; want fresh eyes added and the description kept", r, err)
	}

	sess, err := st.register(registration{role: " Fixer ", repo: "o/r"})
	if err != nil || sess.role != "fixer" || !sess.freshEyes || !strings.Contains(sess.code, "-fixer-") {
		t.Fatalf("register = %+v, %v", sess, err)
	}
	if _, err := st.file(sess, fileInput{title: "t", toRole: "fixr", repo: "o/r"}); err == nil ||
		!strings.Contains(err.Error(), `no role "fixr"; the roles are fixer`) {
		t.Errorf("file to a misspelled role: err = %v, want refused, naming the roles", err)
	}
}

// TestBeliefsReachTheirScope covers which beliefs reach a session, and the
// gate: a session's proposal reaches nobody until you accept it.
func TestBeliefsReachTheirScope(t *testing.T) {
	st := newStore(t)
	for _, b := range [][3]string{
		{"Everyone, everywhere.", "", ""},
		{"Fixers, everywhere.", "fixer", ""},
		{"Everyone on app.", "", "owner/app"},
		{"Fixers on app.", "fixer", "owner/app"},
		{"Fixers elsewhere.", "fixer", "owner/other"},
		{"Auditors only.", "auditor", ""},
	} {
		if _, err := st.believe(nil, b[0], b[1], b[2], "", ""); err != nil {
			t.Fatal(err)
		}
	}
	fixer := mustRegister(t, st, "fixer")
	told := func() []string {
		t.Helper()
		got, err := st.beliefsFor(fixer)
		if err != nil {
			t.Fatal(err)
		}
		return statements(got)
	}
	want := []string{"Everyone, everywhere.", "Everyone on app.", "Fixers, everywhere.", "Fixers on app."}
	if got := told(); !reflect.DeepEqual(got, want) {
		t.Errorf("a fixer on app is told %q\nwant %q", got, want)
	}
	if got, err := st.beliefsFor(nil); err != nil || !reflect.DeepEqual(statements(got), []string{"Everyone, everywhere."}) {
		t.Errorf("before registering: %q, %v; want only the belief for everyone, everywhere", got, err)
	}

	if _, err := st.believe(fixer, "Proposed rule.", "fixer", "", "", ""); err == nil {
		t.Error("a proposal without a note was accepted")
	}
	proposal, err := st.believe(fixer, "Proposed rule.", "fixer", "", "The owner said so.", "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(told(), "Proposed rule.") {
		t.Error("a proposal reached a session before it was accepted")
	}
	if _, _, _, err := st.settleBelief(proposal, "retire"); err == nil {
		t.Error("retired a belief that was never accepted")
	}
	if _, _, _, err := st.settleBelief(proposal[len(proposal)-4:], "accept"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(told(), "Proposed rule.") {
		t.Error("an accepted proposal didn't reach the session")
	}
	if _, _, _, err := st.settleBelief(proposal, "retire"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(told(), "Proposed rule.") {
		t.Error("a retired belief still reached the session")
	}

	doomed, err := st.believe(fixer, "Bad idea.", "", "", "It seemed right at the time.", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.settleBelief(doomed, "reject"); err != nil {
		t.Fatal(err)
	}
	if one(t, st.db, "select count(*) from belief where code_name = ?", doomed) != "0" ||
		one(t, st.db, "select count(*) from history__belief where code_name = ? and history_triggering_action_code = 'D'", doomed) != "1" {
		t.Error("a rejected proposal should be deleted, with history keeping it")
	}

	rows, err := st.beliefs("accepted", "fixer", "owner/app")
	if err != nil || len(rows) != 4 {
		t.Errorf("beliefs reaching a fixer on app = %d, %v; want 4", len(rows), err)
	}
	if rows, err := st.beliefs("all", "", ""); err != nil || len(rows) != 7 || rows[len(rows)-1].status != "retired" {
		t.Errorf("all beliefs = %v, %v; want 7, the retired one last", rows, err)
	}
	if _, err := st.beliefs("accepted", "fixr", ""); err == nil {
		t.Error("listed beliefs for a role nobody defined")
	}
}

// TestReplacingAHandoff: a handoff that supersedes an earlier one closes it,
// so the next session reads one open handoff and nothing before it.
func TestReplacingAHandoff(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	handoff := func(title, replaces string) fileInput {
		return fileInput{typ: "handoff", title: title, body: "where things stand", toRole: "fixer",
			repo: "owner/app", replaces: replaces}
	}
	first := mustFile(t, st, fixer, handoff("Round one", ""))
	if _, err := st.file(fixer, fileInput{typ: "improvement", title: "Nightly fuzzing", toRole: "fixer",
		repo: "owner/app", replaces: first}); err == nil {
		t.Error("an improvement replaced a handoff; only the same type should")
	}
	second := mustFile(t, st, fixer, handoff("Round two", first[len(first)-4:]))

	was, err := st.show(nil, first, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if was.status != "done" || !strings.Contains(was.resolution, second) {
		t.Errorf("replaced handoff: status %q, resolution %q; want done, naming %s", was.status, was.resolution, second)
	}
	now, err := st.show(nil, second, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if now.replaces != first {
		t.Errorf("new handoff replaces %q, want %s", now.replaces, first)
	}
	next := mustRegister(t, st, "fixer")
	rows, err := st.inbox(next, "check the handoff")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].code != second {
		t.Errorf("the next fixer's inbox = %v, want only %s", rows, second)
	}
	if _, err := st.file(next, handoff("Round three", first)); err == nil {
		t.Error("replaced a handoff that was already closed")
	}

	// A session replaces only what it filed or what was addressed to it.
	auditor, other := mustRegister(t, st, "auditor"), mustRegister(t, st, "auditor")
	finding := mustFile(t, st, auditor, fileInput{title: "Theirs", body: "b", toRole: "fixer", repo: "owner/app"})
	if _, err := st.file(other, fileInput{title: "Mine", body: "b", toRole: "fixer", repo: "owner/app", replaces: finding}); err == nil {
		t.Error("an auditor replaced another auditor's finding")
	}
	if _, err := st.file(other, handoff("Not mine", second)); err == nil {
		t.Error("an auditor replaced the fixers' handoff")
	}
	if _, err := st.file(auditor, fileInput{title: "Sharper", body: "b", toRole: "fixer", repo: "owner/app", replaces: finding}); err != nil {
		t.Errorf("an auditor replacing its own finding: %v", err)
	}
}

// TestReplacingABelief: a belief that sharpens another retires it, but only
// once you accept the replacement, so a session is never told both.
func TestReplacingABelief(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	told := func() []string {
		t.Helper()
		got, err := st.beliefsFor(fixer)
		if err != nil {
			t.Fatal(err)
		}
		return statements(got)
	}
	original, err := st.believe(nil, "Docs say what's here now.", "fixer", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sharper, err := st.believe(fixer, "Docs say what's here now, and leave out today and new.", "fixer", "",
		"The owner's words on a dated line in sql/README.md.", original[len(original)-4:])
	if err != nil {
		t.Fatal(err)
	}
	if got := told(); !reflect.DeepEqual(got, []string{"Docs say what's here now."}) {
		t.Errorf("while the replacement is only proposed, the fixer is told %q, want just the original", got)
	}
	code, retired, _, err := st.settleBelief(sharper[len(sharper)-4:], "accept")
	if err != nil {
		t.Fatal(err)
	}
	if code != sharper || retired != original {
		t.Errorf("accept = %s, retired %s; want %s and %s", code, retired, sharper, original)
	}
	if got := told(); !reflect.DeepEqual(got, []string{"Docs say what's here now, and leave out today and new."}) {
		t.Errorf("after accepting the replacement, the fixer is told %q, want only the sharper one", got)
	}
	if _, err := st.believe(nil, "Later still.", "fixer", "", "", original); err == nil {
		t.Error("replaced a belief that was already retired")
	}
	// Yours is in force as you write it, so what it replaces goes at once.
	mine, err := st.believe(nil, "Mine, sharpened.", "fixer", "", "", sharper)
	if err != nil {
		t.Fatal(err)
	}
	if got := told(); !reflect.DeepEqual(got, []string{"Mine, sharpened."}) {
		t.Errorf("after the owner's replacement, the fixer is told %q, want only %s", got, mine)
	}
}

// TestTwoReplacementsOfOneBelief: accepting a second replacement of the same
// belief warns that both are in force.
func TestTwoReplacementsOfOneBelief(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	original, err := st.believe(nil, "Rule.", "fixer", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := st.believe(fixer, "Rule, sharper.", "fixer", "", "why", original)
	second, _ := st.believe(fixer, "Rule, sharper another way.", "fixer", "", "why", original)
	if _, retired, warning, err := st.settleBelief(first, "accept"); err != nil || retired != original || warning != "" {
		t.Errorf("first accept: retired %q, warning %q, %v; want %s retired, no warning", retired, warning, err, original)
	}
	if _, retired, warning, err := st.settleBelief(second, "accept"); err != nil || retired != "" || !strings.Contains(warning, first) {
		t.Errorf("second accept: retired %q, warning %q, %v; want a warning naming %s", retired, warning, err, first)
	}
}

// TestServerRecognizesItsConversation: a server that wasn't told its session
// finds it by the conversation's ID, which is what brains fresh recorded, so
// a session whose server restarts keeps its role, beliefs and filings.
func TestServerRecognizesItsConversation(t *testing.T) {
	st := newStore(t)
	conversation := uuidString(newUUID())
	sess, err := st.register(registration{role: "fixer", repo: "owner/app", externalID: conversation})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", conversation)
	r := mcpExchange(t, st, "", initializeRequest, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		toolCall(3, "inbox", map[string]string{"purpose": "after the server restarted"}))
	told, _ := r["1"]["result"].(map[string]any)["instructions"].(string)
	if !strings.Contains(told, sess.called()+": fixer on owner/app") || !strings.Contains(told, "picked it up again") {
		t.Errorf("instructions = %q, want the session named and the pick-up explained", told)
	}
	if got := toolNames(t, r["2"]); slices.Contains(got, "register") {
		t.Errorf("tools = %v, want register left out: brains already knows the session", got)
	}
	if text, isErr := toolText(t, r["3"]); isErr {
		t.Errorf("inbox without registering = %q, %v; want it served", text, isErr)
	}

	// A conversation brains has never seen is served as a plain session.
	t.Setenv("CLAUDE_CODE_SESSION_ID", uuidString(newUUID()))
	r = mcpExchange(t, st, "", initializeRequest, toolCall(2, "inbox", map[string]string{"purpose": "too soon"}))
	told, _ = r["1"]["result"].(map[string]any)["instructions"].(string)
	if !strings.Contains(told, "Call register once") {
		t.Errorf("instructions = %q, want register asked for", told)
	}
	if text, isErr := toolText(t, r["2"]); !isErr || !strings.Contains(text, "register first") {
		t.Errorf("inbox = %q, %v; want refused", text, isErr)
	}
}

// TestRegisteringRecordsTheConversation: a session that registers itself is
// tied to its conversation too, so the next server recognizes it and brains
// resume can reopen it.
func TestRegisteringRecordsTheConversation(t *testing.T) {
	st := newStore(t)
	conversation := uuidString(newUUID())
	t.Setenv("CLAUDE_CODE_SESSION_ID", conversation)
	r := mcpExchange(t, st, "",
		toolCall(1, "register", map[string]string{"role": "fixer", "repo": "owner/app"}),
		toolCall(2, "register", map[string]string{"role": "auditor", "repo": "owner/app"}),
	)
	text, isErr := toolText(t, r["1"])
	if isErr {
		t.Fatalf("register = %q, %v", text, isErr)
	}
	if strings.Contains(text, "register with resume") {
		t.Errorf("register = %q; want no resume advice, since a restarted server recognizes the conversation", text)
	}
	sess, _, err := sessionByConversation(st.db, conversation)
	if err != nil || sess == nil {
		t.Fatalf("sessionByConversation = %v, %v; want the session it just registered", sess, err)
	}
	if text, isErr := toolText(t, r["2"]); !isErr || !strings.Contains(text, "already knows this") {
		t.Errorf("a second register = %q, %v; want refused, naming the session", text, isErr)
	}
	if again, _, err := findSession(st.db, sess.nickname); err != nil || again.externalID != conversation {
		t.Errorf("resuming by nickname = %+v, %v; want the conversation recorded", again, err)
	}
}

func TestWrap(t *testing.T) {
	if got, want := wrap("one two three\nfour", "  ", 9), "  one two\n  three\n  four\n"; got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}
}

// TestOldWorkLeavesView: a task closed more than 30 days ago drops out of
// lists and endings unless it stands as a decision, and a session does when
// it's done nothing for 30 days. Full codes still reach both.
func TestOldWorkLeavesView(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	file := func(title string) string {
		return mustFile(t, st, fixer, fileInput{title: title, body: "b", toRole: "fixer", repo: "owner/app"})
	}
	fixed, decided, lately := file("Fixed long ago"), file("Decided long ago"), file("Fixed lately")
	for code, outcome := range map[string]string{fixed: "fixed", decided: "accepted_as_designed", lately: "fixed"} {
		if err := st.close(fixer, code, outcome, "", "Done."); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, st.db, "update task set updated_at = datetime('now', '-31 days') where code_name in (?, ?)", fixed, decided)

	listed := func(old bool) []string {
		t.Helper()
		rows, err := st.list(fixer, "what's settled", "all", "", "", old)
		if err != nil {
			t.Fatal(err)
		}
		var codes []string
		for _, r := range rows {
			codes = append(codes, r.code)
		}
		slices.Sort(codes)
		return codes
	}
	inView, everything := []string{decided, lately}, []string{fixed, decided, lately}
	slices.Sort(inView)
	slices.Sort(everything)
	if got := listed(false); !reflect.DeepEqual(got, inView) {
		t.Errorf("in view = %v, want %v", got, inView)
	}
	if got := listed(true); !reflect.DeepEqual(got, everything) {
		t.Errorf("with old = %v, want %v", got, everything)
	}
	if _, err := st.show(fixer, fixed[len(fixed)-4:], "an old ending", false); err == nil || !strings.Contains(err.Error(), "full code") {
		t.Errorf("ending of a task out of view: err = %v, want a pointer to the full code", err)
	}
	for _, code := range []string{fixed, decided[len(decided)-4:]} {
		if _, err := st.show(fixer, code, "still reachable", false); err != nil {
			t.Errorf("show %s: %v", code, err)
		}
	}

	idle := mustRegister(t, st, "auditor")
	mustExec(t, st.db, "update session set started_at = datetime('now', '-40 days') where pin = ?", idle.pin)
	mustExec(t, st.db, "update action set acted_at = datetime('now', '-40 days') where session_pin = ?", idle.pin)
	sessionCodes := func(old bool) (codes []string) {
		rows, err := st.sessions(old)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			codes = append(codes, r.code)
		}
		return codes
	}
	if got := sessionCodes(false); !reflect.DeepEqual(got, []string{fixer.code}) {
		t.Errorf("sessions in view = %v, want only %s", got, fixer.code)
	}
	if got := sessionCodes(true); len(got) != 2 {
		t.Errorf("sessions with old = %v, want both", got)
	}
	if _, _, err := findSession(st.db, idle.code[len(idle.code)-4:]); err == nil {
		t.Error("an idle session's ending still resolved")
	}
	if _, _, err := findSession(st.db, idle.code); err != nil {
		t.Errorf("an idle session's full code: %v", err)
	}
}

// TestReposByName: a repo brains knows can be named without its owner, in
// any case, as long as only one owner has a repo by that name.
func TestReposByName(t *testing.T) {
	st := newStore(t)
	auth, err := ensureRepo(st.db, "owner/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"app", "App", "OWNER/app"} {
		if pin, err := ensureRepo(st.db, ref); err != nil || pin != auth {
			t.Errorf("ensureRepo(%q) = %d, %v; want %d", ref, pin, err, auth)
		}
	}
	if _, err := ensureRepo(st.db, "unseen"); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("a new repo by name alone: err = %v, want owner/name asked for", err)
	}
	if _, err := ensureRepo(st.db, "someone/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureRepo(st.db, "app"); err == nil || !strings.Contains(err.Error(), "someone/app") {
		t.Errorf("a name two owners share: err = %v, want both named", err)
	}
	if pin, err := ensureRepo(st.db, " owner / app "); err != nil || pin != auth {
		t.Errorf("ensureRepo(%q) = %d, %v; want %d", " owner / app ", pin, err, auth)
	}
	if _, err := ensureRepo(st.db, "own er/app"); err == nil {
		t.Error("added a repo with a space inside its owner")
	}
	if !newRepo(st.db, "owner/ap") || newRepo(st.db, "owner/app") {
		t.Error("newRepo can't tell a repo brains has seen from one it hasn't")
	}
	if _, err := st.list(nil, "", "all", "nobody/nothing", "", false); err == nil {
		t.Error("listed tasks for a repo brains has never seen")
	}
}

// TestLatestSession is what brains resume ROLE REPO finds: the newest
// session in that role on that repo.
func TestLatestSession(t *testing.T) {
	st := newStore(t)
	if _, _, err := latestSession(st.db, "fixer", "owner/app"); err == nil {
		t.Error("found a session before any was registered")
	}
	first := mustRegister(t, st, "fixer")
	mustRegister(t, st, "auditor")
	mustExec(t, st.db, "update session set started_at = datetime('now', '-1 hour') where pin = ?", first.pin)
	second, err := st.register(registration{role: "fixer", repo: "owner/app", externalID: uuidString(newUUID())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.register(registration{role: "fixer", repo: "owner/other"}); err != nil {
		t.Fatal(err)
	}
	sess, _, err := latestSession(st.db, "Fixer", "app")
	if err != nil || sess.code != second.code || sess.externalID != second.externalID {
		t.Errorf("latest fixer on app = %+v, %v; want %s and its conversation", sess, err, second.code)
	}
}

// TestUnstartedSessionsAreForgotten: a session whose server never started is
// deleted after claude exits; one whose server started is kept.
func TestUnstartedSessionsAreForgotten(t *testing.T) {
	st := newStore(t)
	never := mustRegister(t, st, "fixer")
	if forgot, err := st.forgetUnstarted(never); err != nil || !forgot {
		t.Errorf("forgetUnstarted(never started) = %v, %v; want forgotten", forgot, err)
	}
	if n := one(t, st.db, "select count(*) from session where pin = ?", never.pin); n != "0" {
		t.Error("a session that never started is still recorded")
	}
	started := mustRegister(t, st, "fixer")
	mcpExchange(t, st, started.code, initializeRequest)
	if forgot, err := st.forgetUnstarted(started); err != nil || forgot {
		t.Errorf("forgetUnstarted(started) = %v, %v; want kept", forgot, err)
	}
	if started.nickname != "fixer1" {
		t.Errorf("the next fixer is %s; want fixer1, freed by the forgotten one", started.nickname)
	}
}

// TestNicknames: a session is called for its role and the lowest number no
// session in view has, unless it's given a name; a name a role or a session
// in view has is refused; and a number comes free once its session leaves
// view. Nicknames reach sessions wherever codes do.
func TestNicknames(t *testing.T) {
	st := newStore(t)
	register := func(role, nickname string) (*sessionInfo, error) {
		return st.register(registration{role: role, repo: "owner/app", nickname: nickname})
	}
	first, second := mustRegister(t, st, "auditor"), mustRegister(t, st, "auditor")
	fixer := mustRegister(t, st, "fixer")
	named, err := register("auditor", " HM ")
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{first.nickname, second.nickname, fixer.nickname, named.nickname}; !reflect.DeepEqual(got,
		[]string{"auditor1", "auditor2", "fixer1", "hm"}) {
		t.Errorf("nicknames = %v", got)
	}
	for _, bad := range []string{"hm", "auditor2", "fixer", "2nd", "bad name"} {
		if _, err := register("auditor", bad); err == nil {
			t.Errorf("registered a session nicknamed %q", bad)
		}
	}
	if sess, _, err := findSession(st.db, "Auditor2"); err != nil || sess.code != second.code || sess.nickname != "auditor2" {
		t.Errorf("findSession(auditor2) = %+v, %v; want %s", sess, err, second.code)
	}
	if _, err := st.file(named, fileInput{typ: "handoff", title: "For fixer1", toSession: "fixer1"}); err != nil {
		t.Errorf("a handoff to a session by nickname: %v", err)
	}
	if _, err := st.defineRole("hm", "A lens that earned a role.", nil); err == nil {
		t.Error("defined a role named like a session in view")
	}

	mustExec(t, st.db, "update session set started_at = datetime('now', '-40 days') where pin = ?", first.pin)
	mustExec(t, st.db, "update action set acted_at = datetime('now', '-40 days') where session_pin = ?", first.pin)
	again := mustRegister(t, st, "auditor")
	if again.nickname != "auditor1" {
		t.Errorf("after auditor1 left view, the next auditor is %s, want auditor1", again.nickname)
	}
	if sess, _, err := findSession(st.db, "auditor1"); err != nil || sess.code != again.code {
		t.Errorf("findSession(auditor1) = %+v, %v; want the session in view, %s", sess, err, again.code)
	}
}

// TestNewCodesKeepEndingsDistinct: a new code is refused while a code in
// view ends the same way, but not for one that's out of view.
func TestNewCodesKeepEndingsDistinct(t *testing.T) {
	st := newStore(t)
	fixer := mustRegister(t, st, "fixer")
	open := mustFile(t, st, fixer, fileInput{title: "Open", body: "b", toRole: "fixer", repo: "owner/app"})
	old := mustFile(t, st, fixer, fileInput{title: "Old", body: "b", toRole: "fixer", repo: "owner/app"})
	if err := st.close(fixer, old, "fixed", "", "Done."); err != nil {
		t.Fatal(err)
	}
	mustExec(t, st.db, "update task set updated_at = datetime('now', '-31 days') where code_name = ?", old)
	for _, c := range []struct {
		existing string
		want     bool
	}{{open, false}, {old, true}} {
		ending := c.existing[len(c.existing)-4:]
		free, err := codeFree(st.db, "task", taskInView, "20991231-handoff-"+ending, ending)
		if err != nil || free != c.want {
			t.Errorf("an ending shared with %s: free = %v, %v; want %v", c.existing, free, err, c.want)
		}
	}
	if free, err := codeFree(st.db, "task", taskInView, old, "zzzz"); err != nil || free {
		t.Errorf("an exact code out of view: free = %v, %v; want taken", free, err)
	}
}
