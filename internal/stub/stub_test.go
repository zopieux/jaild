package stub

import (
	"reflect"
	"testing"
)

func TestParseNixShellArgs(t *testing.T) {
	inv, err := ParseNixShellArgs([]string{"-p", "hello", "jq", "--run", "hello && jq --version"})
	if err != nil {
		t.Fatalf("ParseNixShellArgs: %v", err)
	}
	if !reflect.DeepEqual(inv.Packages, []string{"hello", "jq"}) || inv.Command != "hello && jq --version" {
		t.Fatalf("unexpected parse result: %#v", inv)
	}

	inv2, err := ParseNixShellArgs([]string{"-p", "ripgrep", "-p", "fd"})
	if err != nil {
		t.Fatalf("ParseNixShellArgs multiple -p: %v", err)
	}
	if !reflect.DeepEqual(inv2.Packages, []string{"ripgrep", "fd"}) || inv2.Command != "" {
		t.Fatalf("unexpected parse result: %#v", inv2)
	}

	if _, err := ParseNixShellArgs([]string{"--pure"}); err == nil {
		t.Fatalf("expected error when no packages specified")
	}
}

func TestParseNixArgs(t *testing.T) {
	inv, err := ParseNixArgs([]string{"shell", "nixpkgs#hello", "nixpkgs#jq", "-c", "hello", "-v"})
	if err != nil {
		t.Fatalf("ParseNixArgs shell: %v", err)
	}
	if inv.Sub != nixSubShell || !reflect.DeepEqual(inv.Packages, []string{"nixpkgs#hello", "nixpkgs#jq"}) || !reflect.DeepEqual(inv.Command, []string{"hello", "-v"}) {
		t.Fatalf("unexpected nix shell parse: %#v", inv)
	}

	invRun, err := ParseNixArgs([]string{"run", "nixpkgs#hello", "--", "--greeting", "hi"})
	if err != nil {
		t.Fatalf("ParseNixArgs run: %v", err)
	}
	if invRun.Sub != nixSubRun || !reflect.DeepEqual(invRun.Packages, []string{"nixpkgs#hello"}) || !reflect.DeepEqual(invRun.Command, []string{"--greeting", "hi"}) {
		t.Fatalf("unexpected nix run parse: %#v", invRun)
	}

	invDevEnv, err := ParseNixArgs([]string{
		"--extra-experimental-features", "nix-command flakes",
		"print-dev-env",
		"--profile", "/home/user/proj/.direnv/flake-profile",
	})
	if err != nil {
		t.Fatalf("ParseNixArgs print-dev-env: %v", err)
	}
	if invDevEnv.Sub != nixSubPrintDevEnv || invDevEnv.Profile != "/home/user/proj/.direnv/flake-profile" || invDevEnv.Installable != "" {
		t.Fatalf("unexpected print-dev-env parse: %#v", invDevEnv)
	}
}
