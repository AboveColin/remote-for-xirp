package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Reviewing an agent's work from a phone means answering two different questions, and
// they need two different calls:
//
//   - "what has it changed but not committed" — the working tree, from git:status and
//     git:fileDiff.
//   - "what has this branch done" — the branch against its base, from git:branchDiff
//     and git:branchFileDiff.
//
// A session usually has both, so the file list returns both and says which is which.
// The daemon hands back a ready `unifiedDiff` string, so nothing here computes a diff.
//
// Every git request here passes "git:error" as an error reply type. A deleted worktree
// makes the daemon answer that type (ws/helpers.js resolveGitCwdOrSendError, code
// DIRECTORY_MISSING), and it is not in the responseTypes that api:describe reports for
// most of these, so trusting the catalogue means waiting out the timeout. Measured at
// 75 seconds for this screen's three calls before they named the type.

// diffTextCap bounds a single file's diff.
//
// Measured across the 14 changed files of a real feature branch: 20.3 KB of unified
// diff in total, median 1.3 KB, largest 5.2 KB, none over 20 KB. So 20 KB passes every
// file in that sample untouched and only trips on something no one would read on a
// phone — a lockfile or a generated bundle.
const diffTextCap = 20000

func sessionProject(id string) (projectID, worktree, branch string, err error) {
	res, err := client.Call(map[string]any{"type": "session:get", "sessionId": id}, "session:get", 15*time.Second)
	if err != nil {
		return "", "", "", err
	}
	sm, _ := res["session"].(map[string]any)
	projectID, _ = sm["projectId"].(string)
	worktree, _ = sm["worktreePath"].(string)
	branch, _ = sm["branch"].(string)
	return projectID, worktree, branch, nil
}

// projectBase returns a project's default branch, which is the base a feature branch
// is worth comparing against. It reads the shared projects cache: the changes screen
// and every file diff ask for the same base, and each miss is another serialized
// `projects:list` in front of the diff the screen is waiting for.
func projectBase(projectID string) string {
	return projectsCached().base[projectID]
}

func diffFileList(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, f := range raw {
		if fm, ok := f.(map[string]any); ok {
			out = append(out, project(fm, []string{"path", "status", "additions", "deletions", "staged"}))
		}
	}
	// Biggest change first: that is what you want to look at.
	sort.SliceStable(out, func(i, j int) bool {
		ai, _ := out[i]["additions"].(float64)
		ad, _ := out[i]["deletions"].(float64)
		bi, _ := out[j]["additions"].(float64)
		bd, _ := out[j]["deletions"].(float64)
		return ai+ad > bi+bd
	})
	return out
}

// sessionChanges lists what changed, in both senses.
func sessionChanges(w http.ResponseWriter, r *http.Request, id string) {
	projectID, worktree, branch, err := sessionProject(id)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	if projectID == "" {
		writeJSON(w, 200, map[string]any{"unavailable": "session has no project"})
		return
	}
	out := map[string]any{"branch": branch}

	// Uncommitted work.
	statusReq := map[string]any{"type": "git:status", "projectId": projectID}
	if worktree != "" {
		statusReq["worktreePath"] = worktree
	}
	if res, err := client.Call(statusReq, "git:status", 25*time.Second, "git:error"); err == nil {
		if st, ok := res["status"].(map[string]any); ok {
			files, _ := st["files"].([]any)
			tracked := []any{}
			for _, f := range files {
				// Untracked files have no diff to show and a scratch directory can
				// hold six figures of them; they are counted, not listed.
				if fm, ok := f.(map[string]any); ok {
					if s, _ := fm["status"].(string); s != "?" {
						tracked = append(tracked, f)
					}
				}
			}
			out["worktree"] = map[string]any{
				"files":     diffFileList(tracked),
				"untracked": len(files) - len(tracked),
			}
		}
	}

	// The branch against its base, when they differ.
	base := projectBase(projectID)
	out["base"] = base
	if branch != "" && base != "" && branch != base {
		req := map[string]any{"type": "git:branchDiff", "projectId": projectID, "branch": branch, "base": base}
		if worktree != "" {
			req["worktreePath"] = worktree
		}
		if res, err := client.Call(req, "git:branchDiff", 30*time.Second, "git:error"); err == nil {
			files, _ := res["files"].([]any)
			out["branchDiff"] = map[string]any{"files": diffFileList(files)}
		} else {
			out["branchDiffError"] = err.Error()
		}
	}

	// A pull request for this branch, if the daemon knows of one.
	if branch != "" && base != "" && branch != base {
		if res, err := client.Call(map[string]any{
			"type": "git:branchPR", "projectId": projectID, "branch": branch,
		}, "git:branchPR", 20*time.Second); err == nil {
			if pr, ok := res["pr"].(map[string]any); ok {
				out["pr"] = project(pr, []string{"number", "title", "url", "state", "isDraft"})
			}
		}
	}

	writeJSON(w, 200, out)
}

// sessionFileDiff returns one file's unified diff, either the uncommitted changes or
// the branch's changes against its base.
func sessionFileDiff(w http.ResponseWriter, r *http.Request, id string) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, 400, map[string]any{"error": "missing path"})
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "worktree"
	}

	projectID, worktree, branch, err := sessionProject(id)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	if projectID == "" {
		writeJSON(w, 404, map[string]any{"error": "session has no project"})
		return
	}

	var req map[string]any
	var want string
	switch mode {
	case "branch":
		base := projectBase(projectID)
		if base == "" || branch == "" || base == branch {
			writeJSON(w, 400, map[string]any{"error": "no base branch to compare against"})
			return
		}
		req = map[string]any{
			"type": "git:branchFileDiff", "projectId": projectID,
			"branch": branch, "base": base, "file": path,
		}
		want = "git:branchFileDiff"
	default:
		req = map[string]any{"type": "git:fileDiff", "projectId": projectID, "file": path}
		want = "git:fileDiff"
	}
	if worktree != "" {
		req["worktreePath"] = worktree
	}

	res, err := client.Call(req, want, 30*time.Second, "git:error")
	if err != nil {
		writeJSON(w, 200, map[string]any{"unavailable": err.Error()})
		return
	}
	diff, _ := res["unifiedDiff"].(string)
	truncated := false
	if len(diff) > diffTextCap {
		// Cut on a line boundary so the last line shown is a whole line.
		cut := strings.LastIndex(diff[:diffTextCap], "\n")
		if cut < 0 {
			cut = diffTextCap
		}
		diff = diff[:cut]
		truncated = true
	}
	writeJSON(w, 200, map[string]any{
		"file":      path,
		"mode":      mode,
		"diff":      diff,
		"truncated": truncated,
	})
}
