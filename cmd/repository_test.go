package cmd

import (
	"io"
	"os"
	"strings"
	"testing"

	. "knative.dev/kn-plugin-func/testing"
)

// TestRepository_List ensures that the 'list' subcommand shows the client's
// set of repositories by name for builtin repositories, by explicitly
// setting the repositories' path to a new path which includes no others.
func TestRepository_List(t *testing.T) {
	defer WithEnvVar(t, "XDG_CONFIG_HOME", t.TempDir())()
	cmd := NewRepositoryListCmd(NewClient)
	cmd.SetArgs([]string{}) // Do not use test command args

	// Execute the command, capturing the output sent to stdout
	stdout := piped(t)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// Assert the output matches expected (whitespace trimmed)
	expect := "default"
	output := stdout()
	if output != expect {
		t.Fatalf("expected:\n'%v'\ngot:\n'%v'\n", expect, output)
	}
}

// TestRepository_Add ensures that the 'add' subcommand accepts its positional
// arguments, respects the repositories' path flag, and the expected name is echoed
// upon subsequent 'list'.
func TestRepository_Add(t *testing.T) {
	defer WithEnvVar(t, "XDG_CONFIG_HOME", t.TempDir())()
	var (
		add    = NewRepositoryAddCmd(NewClient)
		list   = NewRepositoryListCmd(NewClient)
		stdout = piped(t)
	)
	// Do not use test command args
	add.SetArgs([]string{})
	list.SetArgs([]string{})

	// add [flags] <old> <new>
	add.SetArgs([]string{
		"newrepo",
		TestRepoURI("repository", t),
	})

	// Parse flags and args, performing action
	if err := add.Execute(); err != nil {
		t.Fatal(err)
	}

	// List post-add, capturing output from stdout
	if err := list.Execute(); err != nil {
		t.Fatal(err)
	}

	// Assert the list output now includes the name from args (whitespace trimmed)
	expect := "default\nnewrepo"
	output := stdout()
	if output != expect {
		t.Fatalf("expected:\n'%v'\ngot:\n'%v'\n", expect, output)
	}
}

// TestRepository_Rename ensures that the 'rename' subcommand accepts its
// positional arguments, respects the repositories' path flag, and the name is
// reflected as having been renamed upon subsequent 'list'.
func TestRepository_Rename(t *testing.T) {
	defer WithEnvVar(t, "XDG_CONFIG_HOME", t.TempDir())()
	var (
		add    = NewRepositoryAddCmd(NewClient)
		rename = NewRepositoryRenameCmd(NewClient)
		list   = NewRepositoryListCmd(NewClient)
		stdout = piped(t)
	)
	// Do not use test command args
	add.SetArgs([]string{})
	rename.SetArgs([]string{})
	list.SetArgs([]string{})

	// add a repo which will be renamed
	add.SetArgs([]string{"newrepo", TestRepoURI("repository", t)})
	if err := add.Execute(); err != nil {
		t.Fatal(err)
	}

	// rename [flags] <old> <new>
	rename.SetArgs([]string{
		"newrepo",
		"renamed",
	})

	// Parse flags and args, performing action
	if err := rename.Execute(); err != nil {
		t.Fatal(err)
	}

	// List post-rename, capturing output from stdout
	if err := list.Execute(); err != nil {
		t.Fatal(err)
	}

	// Assert the list output now includes the name from args (whitespace trimmed)
	expect := "default\nrenamed"
	output := stdout()
	if output != expect {
		t.Fatalf("expected:\n'%v'\ngot:\n'%v'\n", expect, output)
	}
}

// TestRepository_Remove ensures that the 'remove' subcommand accepts name as
// its argument, respects the repository's flag, and the entry is removed upon
// subsequent 'list'.
func TestRepository_Remove(t *testing.T) {
	defer WithEnvVar(t, "XDG_CONFIG_HOME", t.TempDir())()
	var (
		add    = NewRepositoryAddCmd(NewClient)
		remove = NewRepositoryRemoveCmd(NewClient)
		list   = NewRepositoryListCmd(NewClient)
		stdout = piped(t)
	)
	// Do not use test command args
	add.SetArgs([]string{})
	remove.SetArgs([]string{})
	list.SetArgs([]string{})

	// add a repo which will be removed
	add.SetArgs([]string{"newrepo", TestRepoURI("repository", t)})
	if err := add.Execute(); err != nil {
		t.Fatal(err)
	}

	// remove [flags] <name>
	remove.SetArgs([]string{
		"newrepo",
	})

	// Parse flags and args, performing action
	if err := remove.Execute(); err != nil {
		t.Fatal(err)
	}

	// List post-remove, capturing output from stdout
	if err := list.Execute(); err != nil {
		t.Fatal(err)
	}

	// Assert the list output now includes the name from args (whitespace trimmed)
	expect := "default"
	output := stdout()
	if output != expect {
		t.Fatalf("expected:\n'%v'\ngot:\n'%v'\n", expect, output)
	}
}

// Helpers
// -------

// pipe the output of stdout to a buffer whose value is returned
// from the returned function.  Call pipe() to start piping output
// to the buffer, call the returned function to access the data in
// the buffer.
func piped(t *testing.T) func() string {
	t.Helper()
	var (
		o = os.Stdout
		c = make(chan error, 1)
		b strings.Builder
	)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = w

	go func() {
		_, err := io.Copy(&b, r)
		r.Close()
		c <- err
	}()

	return func() string {
		os.Stdout = o
		w.Close()
		err := <-c
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(b.String())
	}
}
