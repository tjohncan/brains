package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// TestCompletion: what bash is offered for a command line, with the cursor at
// its end.
func TestCompletion(t *testing.T) {
	db, path := newDB(t)
	st := &store{db: db}
	mustDefine(t, st, "fixer", "You fix.", false)
	mustDefine(t, st, "auditor", "You audit.", true)
	fixer := mustRegister(t, st, "fixer") // fixer1, on owner/app
	auditor := mustRegister(t, st, "auditor")
	if _, err := ensureRepo(db, "someone/lib"); err != nil {
		t.Fatal(err)
	}
	open := mustFile(t, st, auditor, fileInput{title: "Open", body: "b", toRole: "fixer", repo: "owner/app"})
	done := mustFile(t, st, auditor, fileInput{title: "Done", body: "b", toRole: "fixer", repo: "owner/app"})
	if err := st.close(fixer, done, "fixed", "", "Fixed."); err != nil {
		t.Fatal(err)
	}
	accepted, err := st.believe(nil, "Accepted.", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := st.believe(fixer, "Proposed.", "", "", "Why.", "")
	if err != nil {
		t.Fatal(err)
	}
	end := func(code string) string { return code[strings.LastIndex(code, "-")+1:] }

	for line, want := range map[string][]string{
		"brains ":                              commands,
		"brains re":                            {"reject", "resume", "retire"},
		"brains fresh ":                        {"auditor", "fixer"},
		"brains fresh -":                       {"-as"},
		"brains fresh -as hm f":                {"fixer"},
		"brains fresh fixer ":                  {"owner/app", "someone/lib"},
		"brains fresh fixer l":                 {"lib"},
		"brains fresh fixer owner/app ":        nil,
		"brains resume ":                       {"auditor", "auditor1", "fixer", "fixer1"},
		"brains resume fixer ":                 {"owner/app", "someone/lib"},
		"brains resume fixer1 ":                nil,
		"brains resume 2":                      {fixer.code, auditor.code},
		"brains task ":                         {end(open), end(done)},
		"brains task 20":                       {open, done},
		"brains close ":                        {end(open)},
		"brains close " + end(open) + " ":      {"-dup-of", "-outcome", "-resolution"},
		"brains close x -outcome ":             outcomes,
		"brains close x -dup-of ":              {end(open), end(done)},
		"brains tasks ":                        {"-old", "-repo", "-status", "-type"},
		"brains tasks -status ":                {"active", "open", "claimed", "done", "dropped", "all"},
		"brains tasks -type ":                  taskTypes,
		"brains tasks -repo own":               {"owner/app"},
		"brains beliefs -status ":              {"accepted", "proposed", "retired", "all"},
		"brains file -to ":                     {"fixer"},
		"brains file -to-session ":             {"fixer1"},
		"brains file -replaces ":               {end(open)},
		`brains believe "A rule" -`:            {"-note", "-repo", "-replaces", "-role"},
		`brains believe "A rule" -replaces `:   {end(accepted)},
		"brains accept ":                       {end(proposed)},
		"brains accept " + end(proposed) + " ": nil,
		"brains retire ":                       {end(accepted)},
		"brains belief ":                       {end(accepted), end(proposed)},
		"brains role ":                         {"auditor", "fixer"},
		"brains role fixer ":                   {"-description", "-fresh-eyes"},
		`brains believe "unclosed`:             nil,
		"brains tasks -status=":                nil,
	} {
		want = slices.Sorted(slices.Values(want))
		if got := complete(st, line); !slices.Equal(got, want) {
			t.Errorf("complete(%q) = %q, want %q", line, got, want)
		}
	}

	// Without a database, only what doesn't come from one.
	if got := complete(nil, "brains ta"); !slices.Equal(got, []string{"task", "tasks"}) {
		t.Errorf("complete without a database = %q, want task and tasks", got)
	}
	if got := complete(nil, "brains task "); got != nil {
		t.Errorf("task codes without a database = %q, want none", got)
	}

	// bash gives the whole line and where the cursor is in it.
	t.Setenv("BRAINS_DB", path)
	var out bytes.Buffer
	runComplete(&out, "brains tas -old", "10")
	if out.String() != "task\ntasks\n" {
		t.Errorf("runComplete = %q, want task and tasks, one per line", out.String())
	}
}
