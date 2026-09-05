import json, base64
MKBODY_B64=base64.b64encode(open('/home/jhp1403/.claude/jobs/1df1e5e7/tmp/mkbody.go','rb').read()).decode()
GK="http://codearmory-gatekeeper:8081"
GF="http://codearmory-git-factory:9002"
BOT_USER="codearmory-bot"

# One forge/run step. Everything over curl (Go's http client can't reach the platform
# services from the sandbox — measured on the pr-review report step). Bot identity: log
# in with the bot's password (a secret: ref) to get a session that carries the bot's
# granted permissions, then open the PR as the bot. Author = the authenticated caller
# (git-factory api_pulls.go), so the PR shows codearmory-bot, not the triggering user.
open_cmd = (
 "set -e; export HOME=/tmp GOFLAGS=-buildvcs=false; mkdir -p /tmp/.openpr; "
 f"GK={GK}; GF={GF}; BOT_USER={BOT_USER}; "
 # title/body arrive base64 (arbitrary text, safe past the shell) -> files
 "printf '%s' '${inputs.title_b64}' | base64 -d > /tmp/.openpr/title.txt; "
 "printf '%s' '${inputs.body_b64}' | base64 -d > /tmp/.openpr/body.txt; "
 # 1) log in as the bot -> session token
 "LOGIN=$(curl -s --max-time 30 --retry 3 --retry-delay 2 -X POST \"$GK/login\" -H 'content-type: application/json' "
 "-d \"{\\\"username\\\":\\\"$BOT_USER\\\",\\\"password\\\":\\\"$BOT_PW\\\"}\"); "
 "SESS=$(printf '%s' \"$LOGIN\" | sed -E 's/.*\"token\":\"([^\"]+)\".*/\\1/'); "
 "[ -n \"$SESS\" ] && [ \"$SESS\" != \"$LOGIN\" ] || { echo \"bot login failed\"; exit 1; }; "
 # 2) resolve repo id via the native by-path POST endpoint
 "RID=$(curl -s --max-time 30 --retry 3 -X POST \"$GF/repos/by-path\" -H \"authorization: Bearer $SESS\" "
 "-H 'content-type: application/json' -d \"{\\\"namespace\\\":\\\"${inputs.namespace}\\\",\\\"name\\\":\\\"${inputs.repo}\\\"}\" "
 "| sed -E 's/.*\"id\":\"([^\"]+)\".*/\\1/'); "
 "[ -n \"$RID\" ] || { echo 'resolve repo failed'; exit 1; }; "
 # 3) build the PR body JSON safely (Go, file-only — no network)
 f"echo '{MKBODY_B64}' | base64 -d > /tmp/.openpr/mkbody.go; "
 "go run /tmp/.openpr/mkbody.go /tmp/.openpr/title.txt /tmp/.openpr/body.txt "
 "'${inputs.source_ref}' '${inputs.target_ref}' /tmp/.openpr/pr.json; "
 # 4) create the PR as the bot
 "CODE=$(curl -s -o /tmp/.openpr/resp.json --max-time 60 --retry 3 --retry-all-errors -w '%{http_code}' "
 "-X POST \"$GF/repos/$RID/pulls\" -H \"authorization: Bearer $SESS\" -H 'content-type: application/json' "
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
 "description":"Open a git-factory pull request, authored by the codearmory-bot account. "
   "Wrapper around a single forge/run step so it reuses the proven curl credential path "
   "instead of a native run-role token (which can't carry cross-namespace repo grants). "
   "Inputs: namespace, repo, title_b64, body_b64 (base64 so arbitrary text survives the "
   "shell), source_ref (the branch), target_ref (base, e.g. dev). Outputs pr_number.",
 "inputs":[
   {"name":"namespace","required":True,"description":"repo owner namespace (e.g. ops)"},
   {"name":"repo","required":True,"description":"repo name within the namespace"},
   {"name":"title_b64","required":True,"description":"base64 of the PR title"},
   {"name":"body_b64","required":True,"description":"base64 of the PR body (markdown)"},
   {"name":"source_ref","required":True,"description":"the branch being merged"},
   {"name":"target_ref","required":False,"description":"base branch merged into (default dev)","default":"dev"},
 ],
 "steps":[
   {"name":"open","action":"forge/run","timeout":240,
    "with":{"image":"golang:1.25","runner_class":"large",
      "secret_refs":{"BOT_PW":"secret:codearmory-bot-password"},
      "run":open_cmd,"output_env":["PR_NUMBER"]}},
 ],
 "maps":[],
 "routes":[],
 "outputs":[{"name":"pr_number","value":"${steps.open.output.PR_NUMBER}"}],
}
open('/home/jhp1403/.claude/jobs/1df1e5e7/tmp/git-open-pr.workflow.json','w').write(json.dumps(wf,indent=2))
print("wrote git-open-pr.workflow.json | steps:", [s['name'] for s in wf['steps']], "| inputs:", [i['name'] for i in wf['inputs']])
