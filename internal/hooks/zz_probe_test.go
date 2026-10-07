package hooks

import (
	"fmt"
	"testing"
)

func TestProbeLoad(t *testing.T) {
	cfg := Load("/tmp/zzperm")
	fmt.Printf("config: %+v\n", cfg)
	r := Run(cfg, PermissionRequest, map[string]any{
		"tool_name": "Write", "hook_event_name": "PermissionRequest",
	}, "/tmp/zzperm")
	fmt.Printf("res: %+v\n", r)
}
