package cipolicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Execute the actual privileged workflow shell, with gh and sleep replaced by
// local fixtures. No repository code or GitHub service is contacted.
func TestDependabotDispatchProtocol(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/dependabot-auto-merge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	script := wf.Jobs["merge"].Steps[0].Run
	for _, tc := range []struct {
		name    string
		success bool
		posts   int
		want    string
	}{
		{"returned_id", true, 1, "Dispatched full CI run"},
		{"returned_id_ref", true, 1, "Dispatched full CI run"},
		{"merge_put", true, 1, "Dispatched full CI run"},
		{"empty_response", true, 1, "Dispatched full CI run"},
		{"lost_response", true, 1, "Dispatched full CI run"},
		{"malformed_response", true, 1, "Dispatched full CI run"},
		{"delayed_list", true, 1, "Dispatched full CI run"},
		{"delayed_get", true, 1, "Dispatched full CI run"},
		{"get_malformed_then_good", true, 1, "Dispatched full CI run"},
		{"get_partial_then_good", true, 1, "Dispatched full CI run"},
		{"invalid_id", true, 1, "Dispatched full CI run"},
		{"zero", false, 1, "reconcile"},
		{"multiple", false, 1, "reconcile"},
		{"wrong_event", false, 1, "reconcile"},
		{"wrong_branch", false, 1, "reconcile"},
		{"wrong_sha", false, 1, "reconcile"},
		{"wrong_workflow", false, 1, "reconcile"},
		{"wrong_repo", false, 1, "reconcile"},
		{"old_run", false, 1, "reconcile"},
		{"incomplete_list", false, 1, "reconcile"},
		{"overflow_list", false, 1, "reconcile"},
		{"malformed_list", false, 1, "reconcile"},
		{"malformed_candidate", false, 1, "reconcile"},
		{"id_wrong_event", false, 1, "does not match"},
		{"id_wrong_branch", false, 1, "does not match"},
		{"id_wrong_sha", false, 1, "does not match"},
		{"id_wrong_workflow", false, 1, "does not match"},
		{"id_wrong_repo", false, 1, "does not match"},
		{"id_wrong_path", false, 1, "does not match"},
		{"id_wrong_ref", false, 1, "does not match"},
		{"id_wrong_ref_prefix", false, 1, "does not match"},
		{"id_old_run", false, 1, "does not match"},
		{"id_future_run", false, 1, "does not match"},
		{"id_wrong_head_repo", false, 1, "does not match"},
		{"id_wrong_id", false, 1, "does not match"},
		{"get_unavailable", false, 1, "read back"},
		{"main_advanced", true, 0, "Main advanced"},
		{"existing", true, 0, "already owns"},
		{"initial_list_transient", true, 1, "Dispatched full CI run"},
		{"initial_list_bad", false, 0, "Cannot read existing"},
		{"initial_list_incomplete", false, 0, "Cannot read existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range map[string]string{"gh": mockGH, "sleep": "#!/bin/sh\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "-e", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "REPOSITORY=owner/repo", "WORKFLOW_RUN_ID=99", "MOCK_SCENARIO="+tc.name, "MOCK_DIR="+dir)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.success || !strings.Contains(string(output), tc.want) {
				t.Errorf("success=%v error=%v output=%s; want success=%v and %q", err == nil, err, output, tc.success, tc.want)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			posts := 0
			for _, line := range lines {
				if strings.HasPrefix(line, "api --method POST ") {
					posts++
					if !strings.Contains(line, "/dispatches") {
						t.Errorf("unexpected mutation: %s", line)
					}
				}
			}
			if posts != tc.posts {
				t.Errorf("got %d POSTs, want %d; calls=%s", posts, tc.posts, calls)
			}
			if tc.name == "merge_put" && !strings.Contains(string(calls), "api --method PUT repos/owner/repo/pulls/90/merge -f sha=") {
				t.Errorf("qualified PUT merge was not exercised: %s", calls)
			}
			if tc.name == "returned_id" && strings.Contains(string(calls), "/actions/workflows/ci.yml/runs") {
				t.Errorf("returned run ID unexpectedly used fallback listing: %s", calls)
			}
			if tc.name == "empty_response" && !strings.Contains(string(calls), "/actions/workflows/ci.yml/runs") {
				t.Errorf("empty response did not use fallback listing: %s", calls)
			}
			if posts != 0 {
				if payloadOK, err := os.ReadFile(filepath.Join(dir, "payload_ok")); err != nil || string(payloadOK) != "yes" {
					t.Errorf("dispatch payload was not exactly main and landed SHA: %q (%v)", payloadOK, err)
				}
			}
			for scenario, counter := range map[string]string{"delayed_list": "list", "delayed_get": "get", "get_malformed_then_good": "get", "get_partial_then_good": "get", "initial_list_transient": "initial_list"} {
				if tc.name == scenario {
					value, err := os.ReadFile(filepath.Join(dir, counter))
					if err != nil || string(value) != "3" {
						t.Errorf("expected exactly three %s reads, got %q (%v)", counter, value, err)
					}
				}
			}
		})
	}
}

const mockGH = `#!/usr/bin/env python3
import datetime, json, os, sys
args = sys.argv[1:]
d = os.environ['MOCK_DIR']; scenario = os.environ['MOCK_SCENARIO']
with open(d+'/calls', 'a') as f: f.write(' '.join(args)+'\n')
def emit(x): print(json.dumps(x))
def count(name):
 p=d+'/'+name
 try: n=int(open(p).read())+1
 except FileNotFoundError: n=1
 with open(p,'w') as f: f.write(str(n))
 return n
sha='a'*40; head='b'*40
now=datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
def run():
 r={'id':123,'workflow_id':42,'path':'.github/workflows/ci.yml','event':'workflow_dispatch','head_branch':'main','head_sha':sha,'created_at':now,'repository':{'full_name':'owner/repo'},'head_repository':{'full_name':'owner/repo'}}
 for key, val in [('event','push'),('branch','other'),('sha','c'*40),('workflow',999)]:
  if scenario in ('wrong_'+key, 'id_wrong_'+key): r[{'branch':'head_branch','sha':'head_sha','workflow':'workflow_id'}.get(key,key)]=val
 if scenario in ('wrong_repo','id_wrong_repo'): r['repository']['full_name']='other/repo'
 if scenario in ('old_run','id_old_run'): r['created_at']='2020-01-01T00:00:00Z'
 if scenario=='id_future_run': r['created_at']='2099-01-01T00:00:00Z'
 if scenario=='returned_id_ref': r['path'] += '@main'
 if scenario=='id_wrong_path': r['path']='.github/workflows/other.yml'
 if scenario=='id_wrong_ref': r['path'] += '@other'
 if scenario=='id_wrong_ref_prefix': r['path'] += '@main-extra'
 if scenario=='id_wrong_head_repo': r['head_repository']['full_name']='other/repo'
 if scenario=='id_wrong_id': r['id']=124
 return r
if args[0]=='run':
 n=count('initial_list')
 if scenario=='initial_list_bad' or (scenario=='initial_list_transient' and n<3): print('{'); sys.exit(0)
 if scenario=='initial_list_incomplete': emit([{}])
 elif scenario=='existing': emit([{'databaseId':122,'headSha':sha,'status':'queued','event':'push','conclusion':None,'createdAt':now,'url':'https://example.invalid'}])
 else: emit([])
elif '/actions/runs/99' in ' '.join(args): emit({'event':'pull_request','actor':{'login':'dependabot[bot]'},'head_repository':{'full_name':'owner/repo'},'head_branch':'dependabot/test','head_sha':head,'pull_requests':[{'number':90}]})
elif '/pulls/90/merge' in ' '.join(args):
 if args[:3]!=['api','--method','PUT'] or 'sha='+head not in args or 'merge_method=squash' not in args: sys.exit(3)
 emit({'merged':True,'sha':sha})
elif '/pulls/90' in ' '.join(args): emit({'number':90,'state':'open','merged':scenario!='merge_put','merge_commit_sha':sha,'user':{'login':'dependabot[bot]'},'head':{'repo':{'full_name':'owner/repo'},'ref':'dependabot/test','sha':head},'base':{'repo':{'full_name':'owner/repo'},'ref':'main'}})
elif '/git/ref/heads/main' in ' '.join(args): print('c'*40 if scenario=='main_advanced' else sha)
elif '/actions/workflows/ci.yml/dispatches' in ' '.join(args):
 payload=json.load(sys.stdin)
 if payload != {'ref':'main','inputs':{'expected_sha':sha}}: sys.exit(3)
 with open(d+'/payload_ok','w') as f: f.write('yes')
 if scenario=='lost_response': sys.exit(1)
 if scenario=='malformed_response': print('{')
 elif scenario=='invalid_id': emit({'workflow_run_id':'123/../../cancel'})
 elif scenario in ('returned_id','returned_id_ref','merge_put','delayed_get','get_unavailable','get_malformed_then_good','get_partial_then_good','id_wrong_event','id_wrong_branch','id_wrong_sha','id_wrong_workflow','id_wrong_repo','id_wrong_path','id_wrong_ref','id_wrong_ref_prefix','id_old_run','id_future_run','id_wrong_head_repo','id_wrong_id'): emit({'workflow_run_id':123,'run_url':'https://api.github.com/repos/owner/repo/actions/runs/123','html_url':'https://github.com/owner/repo/actions/runs/123'})
elif '/actions/workflows/ci.yml/runs' in ' '.join(args):
 if not all(x in args for x in ['branch=main','event=workflow_dispatch','per_page=100']): sys.exit(3)
 n=count('list'); rows=[] if scenario in ('zero',) or (scenario=='delayed_list' and n<3) else [run()]
 if scenario=='multiple': rows.append(dict(run(),id=124))
 if scenario=='malformed_candidate': rows.append(dict(run(),id=None))
 if scenario=='overflow_list': rows=[dict(run(),id=i+1) for i in range(101)]
 if scenario=='malformed_list': print('{')
 else: emit({'total_count':len(rows)+(1 if scenario=='incomplete_list' else 0),'workflow_runs':rows})
elif '/actions/workflows/ci.yml' in ' '.join(args): emit({'id':42,'path':'.github/workflows/ci.yml'})
elif '/actions/runs/123' in ' '.join(args):
 n=count('get')
 if scenario=='get_unavailable' or (scenario=='delayed_get' and n<3): sys.exit(1)
 if scenario=='get_malformed_then_good' and n<3: print('{'); sys.exit(0)
 if scenario=='get_partial_then_good' and n<3: emit({'id':123}); sys.exit(0)
 emit(run())
else:
 print('unexpected gh call',file=sys.stderr); sys.exit(2)
`
