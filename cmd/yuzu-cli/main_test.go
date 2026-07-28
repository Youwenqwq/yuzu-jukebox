package main

import "testing"

func TestCommandForArgsRoutesNestedCommands(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantUsage string
		wantRest  int
	}{
		{name: "three levels", args: []string{"integration", "scope", "bind", "bridge", "adapter", "group", "42", "lobby"}, wantUsage: "integration scope bind <integration_id> <adapter_id> <scope_type> <scope_id> <room_id>", wantRest: 5},
		{name: "integration lifecycle", args: []string{"integration", "create", "bridge", "Bridge"}, wantUsage: "integration create <id> <name>", wantRest: 2},
		{name: "room controller", args: []string{"room", "controller", "grant", "lobby", "principal-1"}, wantUsage: "room controller grant <room> <principal_id>", wantRest: 2},
		{name: "two levels", args: []string{"principal", "list", "alice"}, wantUsage: "principal list [query] [-limit 50]", wantRest: 1},
		{name: "identity bind code", args: []string{"identity", "bind-code"}, wantUsage: "identity bind-code", wantRest: 0},
		{name: "existing command", args: []string{"queue", "del", "lobby", "entry-1"}, wantUsage: "queue del <room> <entry_id>", wantRest: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, rest, ok := commandForArgs(tt.args)
			if !ok {
				t.Fatal("command was not routed")
			}
			if cmd.usage != tt.wantUsage {
				t.Fatalf("usage = %q, want %q", cmd.usage, tt.wantUsage)
			}
			if len(rest) != tt.wantRest {
				t.Fatalf("remaining args = %d, want %d", len(rest), tt.wantRest)
			}
		})
	}
}

func TestManagementCommandsRejectMissingArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "integration create", args: []string{"bridge"}},
		{name: "integration rename", args: []string{"bridge"}},
		{name: "integration enable"},
		{name: "integration disable"},
		{name: "integration rotate-token"},
		{name: "integration delete"},
		{name: "integration scope list"},
		{name: "integration scope bind", args: []string{"bridge", "adapter", "group", "42"}},
		{name: "integration scope unbind", args: []string{"bridge", "adapter", "group", "42"}},
		{name: "integration subject list"},
		{name: "integration subject link", args: []string{"bridge", "adapter", "group", "42", "user-1"}},
		{name: "integration subject unlink", args: []string{"bridge", "adapter", "group", "42", "user-1"}},
		{name: "room controller list"},
		{name: "room controller grant", args: []string{"lobby"}},
		{name: "room controller revoke", args: []string{"lobby"}},
		{name: "player bind", args: []string{"speaker-1"}},
		{name: "player unbind", args: []string{"speaker-1"}},
		{name: "room players"},
		{name: "room volume", args: []string{"lobby"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := commands[tt.name].run(tt.args)
			if err == nil {
				t.Fatal("missing arguments were accepted")
			}
			want := errUsage(tt.name).Error()
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
		})
	}
}

func TestIdentityBindCodeUsageIsRegistered(t *testing.T) {
	cmd, ok := commands["identity bind-code"]
	if !ok {
		t.Fatal("identity bind-code command is not registered")
	}
	if cmd.usage != "identity bind-code" {
		t.Fatalf("usage = %q", cmd.usage)
	}
	if _, ok := groupMeta["identity"]; !ok {
		t.Fatal("identity command group is not registered")
	}
	children := groupChildren("identity")
	if len(children) != 1 || children[0] != "bind-code" {
		t.Fatalf("identity group children = %#v", children)
	}
}
