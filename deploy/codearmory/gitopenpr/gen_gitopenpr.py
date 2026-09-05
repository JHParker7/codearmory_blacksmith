import json, base64
MKBODY_B64=base64.b64encode(open('/home/jhp1403/.claude/jobs/1df1e5e7/tmp/mkbody.go','rb').read()).decode()
BOT_ID=open('/home/jhp1403/.claude/jobs/1df1e5e7/tmp/bot_userid').read().strip()
GF="http://codearmory-git-factory:9002"
GITURL="git:http://codearmory-git-factory:9002/${inputs.namespace}/${inputs.repo}.git"

# One forge/run step. Option A: it authenticates with the run's OWN per-repo git:
# credential (the same token the pr-review report step uses, scoped to just this repo
# and minted fresh per run) -- NOT a standing bot credential. The PR is ATTRIBUTED to
# the automation bot via git-factory's createPull `author` override (allowlisted
# server-side by GIT_FACTORY_AUTOMATION_USERS), so the "opened by" shows the bot while
# the real caller stays recorded (OpenedByUserID) for accountability. Everything over
# curl: Go's http client can't reach git-factory from the sandbox (report-step finding).
open_cmd = (
 "set -e; export HOME=/tmp GOFLAGS=-buildvcs=false; mkdir -p /tmp/.openpr; "
 f"GF={GF}; "
 # the per-run credential is embedded in the git: secret URL; pull the token out of it
 "TOK=$(printf '%s' \"$GIT_URL\" | sed -E 's#.*//[^:/]+:([^@]+)@.*#\\1#'); "
 "[ -n \"$TOK\" ] || { echo 'no run token in GIT_URL'; exit 1; }; "
 # title/body arrive base64 (arbitrary text, safe past the shell) -> files
 "printf '%s' '${inputs.title_b64}' | base64 -d > /tmp/.openpr/title.txt; "
 "printf '%s' '${inputs.body_b64}' | base64 -d > /tmp/.openpr/body.txt; "
 # resolve repo id via the native by-path POST endpoint, using the run token
 "RID=$(curl -s --max-time 30 --retry 3 -X POST \"$GF/repos/by-path\" -H \"authorization: Bearer $TOK\" "
 "-H 'content-type: application/json' -d \"{\\\"namespace\\\":\\\"${inputs.namespace}\\\",\\\"name\\\":\\\"${inputs.repo}\\\"}\" "
 "| sed -E 's/.*\"id\":\"([^\"]+)\".*/\\1/'); "
 "[ -n \"$RID\" ] || { echo 'resolve repo failed'; exit 1; }; "
 # build the createPull body JSON safely (Go, file-only), incl. the bot author override
 f"echo '{MKBODY_B64}' | base64 -d > /tmp/.openpr/mkbody.go; "
 "go run /tmp/.openpr/mkbody.go /tmp/.openpr/title.txt /tmp/.openpr/body.txt "
 "'${inputs.source_ref}' '${inputs.target_ref}' '${inputs.author_id}' /tmp/.openpr/pr.json; "
 # create the PR with the run token; git-factory attributes it to the allowlisted bot
 "CODE=$(curl -s -o /tmp/.openpr/resp.json --max-time 60 --retry 3 --retry-all-errors -w '%{http_code}' "
 "-X POST \"$GF/repos/$RID/pulls\" -H \"authorization: Bearer $TOK\" -H 'content-type: application/json' "
 "-d @/tmp/.openpr/pr.json); "
 "cat /tmp/.openpr/resp.json; echo; "
 "case \"$CODE\" in 2*) ;; *) echo \"create PR failed: $CODE\"; exit 1;; esac; "
 "PR_NUMBER=$(sed -E 's/.*\"number\":([0-9]+).*/\\1/' /tmp/.openpr/resp.json); "
 "AUTHOR=$(sed -E 's/.*\"author\":\"([^\"]+)\".*/\\1/' /tmp/.openpr/resp.json); "
 "export PR_NUMBER; echo \"opened PR #$PR_NUMBER author=$AUTHOR\"; echo \"PR_NUMBER=$PR_NUMBER\""
)

wf={
 "name":"git-open-pr",
 "timeout_secs":300,
 "description":"Open a git-factory pull request, attributed to the automation bot. A "
   "single forge/run wrapper that authenticates with the run's own per-repo git: "
   "credential (least privilege -- no standing bot credential) and sets git-factory's "
   "createPull author override so the PR shows the bot, not the triggering user. "
   "Inputs: namespace, repo, title_b64, body_b64 (base64 so arbitrary text survives the "
   "shell), source_ref (the branch), target_ref (base, default dev), author_id (the bot "
   "user id, allowlisted server-side). Output: pr_number.",
 "inputs":[
   {"name":"namespace","required":True,"description":"repo owner namespace (e.g. ops)"},
   {"name":"repo","required":True,"description":"repo name within the namespace"},
   {"name":"title_b64","required":True,"description":"base64 of the PR title"},
   {"name":"body_b64","required":True,"description":"base64 of the PR body (markdown)"},
   {"name":"source_ref","required":True,"description":"the branch being merged"},
   {"name":"target_ref","required":False,"description":"base branch merged into (default dev)","default":"dev"},
   {"name":"author_id","required":False,"description":"automation bot user id for the PR author override","default":BOT_ID},
 ],
 "steps":[
   {"name":"open","action":"forge/run","timeout":240,
    "with":{"image":"golang:1.25","runner_class":"large",
      "secret_refs":{"GIT_URL":GITURL},
      "run":open_cmd,"output_env":["PR_NUMBER"]}},
 ],
 "maps":[],
 "routes":[],
 "outputs":[{"name":"pr_number","value":"${steps.open.output.PR_NUMBER}"}],
}
open('/home/jhp1403/.claude/jobs/1df1e5e7/tmp/git-open-pr.workflow.json','w').write(json.dumps(wf,indent=2))
print("wrote git-open-pr.workflow.json | author default:",BOT_ID,"| inputs:",[i['name'] for i in wf['inputs']])
