package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RateLimitCheckpoint mirrors mod_1221: on rate-limit, commit working-tree
// changes to a Claude-owned ref via plumbing (temp index → write-tree →
// commit-tree) and write .claude/RESUME.md with the resume command.
func RateLimitCheckpoint(cwd, sessionID, trigger string) error {
	root, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil // not a git repo — nothing to checkpoint
	}
	rootStr := strings.TrimSpace(string(root))
	idx, err := os.CreateTemp("", "claude-go-idx-*")
	if err != nil {
		return err
	}
	idxPath := idx.Name()
	idx.Close()
	defer os.Remove(idxPath)

	env := append(os.Environ(),
		"GIT_INDEX_FILE="+idxPath,
		"GIT_AUTHOR_NAME=Claude Code",
		"GIT_AUTHOR_EMAIL=claude@anthropic.com",
		"GIT_COMMITTER_NAME=Claude Code",
		"GIT_COMMITTER_EMAIL=claude@anthropic.com",
	)
	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = rootStr
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// seed index from HEAD then add working changes
	if out, err := run("read-tree", "HEAD"); err != nil {
		// empty repo — start empty index
		_, _ = run("read-tree", "--empty")
		_ = out
	}
	run("add", "-A")
	tree, err := run("write-tree")
	if err != nil {
		return err
	}
	tree = strings.TrimSpace(tree)
	head, _ := exec.Command("git", "-C", rootStr, "rev-parse", "HEAD").Output()
	var commitArgs []string
	commitArgs = append(commitArgs, "commit-tree", tree, "-m", "WIP: Claude Code rate-limit checkpoint")
	if h := strings.TrimSpace(string(head)); h != "" {
		commitArgs = append(commitArgs, "-p", h)
	}
	commit, err := run(commitArgs...)
	if err != nil {
		return err
	}
	ref := "refs/claude-code/checkpoint-" + time.Now().UTC().Format("20060102-150405")
	if _, err := run("update-ref", ref, strings.TrimSpace(commit)); err != nil {
		return err
	}
	// expire checkpoints older than 2 weeks
	pruneOldRefs(rootStr)

	// RESUME.md
	claudeDir := filepath.Join(rootStr, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	resume := fmt.Sprintf(`# Rate-limit checkpoint

Session: %s
Trigger: %s
Ref: %s
Time: %s

Working-tree changes were committed to the ref above. To resume:

  claude --resume %s

To restore files: git checkout %s -- .

You can also use /rewind to restore checkpoints made during the session.
`, sessionID, trigger, ref, time.Now().Format(time.RFC3339), sessionID, ref)
	return os.WriteFile(filepath.Join(claudeDir, "RESUME.md"), []byte(resume), 0o644)
}

func pruneOldRefs(root string) {
	out, err := exec.Command("git", "-C", root, "for-each-ref", "--format=%(refname) %(committerdate:unix)", "refs/claude-code/").Output()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-14 * 24 * time.Hour).Unix()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var name string
		var ts int64
		if _, err := fmt.Sscanf(line, "%s %d", &name, &ts); err == nil && ts < cutoff {
			exec.Command("git", "-C", root, "update-ref", "-d", name).Run()
		}
	}
}
