package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandHelpAndVersionDoNotRequireConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	for _, args := range [][]string{{"--help"}, {"configure", "--help"}, {"config", "--help"}, {"setup", "--help"}, {"install", "codex", "--help"}, {"version"}, {"--version"}} {
		var output bytes.Buffer
		if err := run(append([]string{"--config", path}, args...), strings.NewReader(""), &output); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if output.Len() == 0 {
			t.Fatalf("%v produced no help/version output", args)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("help or version wrote a configuration")
	}
}

func TestConfigFlagBeforeAndAfterCommand(t *testing.T) {
	for _, after := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "中文配置 with spaces.json")
		args := []string{"--config", path, "init"}
		if after {
			args = []string{"init", "--config", path}
		}
		var output bytes.Buffer
		if err := run(args, strings.NewReader(""), &output); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("config flag was ignored: %v", err)
		}
	}
}

func TestUnexpectedArgumentsFailBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{{"unknown-command"}, {"init", "unexpected"}, {"install", "unknown-agent"}, {"install", "codex", "unexpected"}, {"configure", "unexpected"}} {
		var output bytes.Buffer
		if err := run(append([]string{"--config", path}, args...), strings.NewReader(""), &output); err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid command wrote a configuration")
	}
}

func TestConfigureRejectsNoninteractiveInputWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var output bytes.Buffer
	if err := run([]string{"configure", "--config", path}, strings.NewReader(""), &output); err == nil {
		t.Fatal("configure accepted non-terminal input")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("non-terminal configure wrote a configuration")
	}
}
