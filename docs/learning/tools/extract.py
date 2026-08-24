import json, os, glob, sys
from collections import Counter

SESS_DIR = os.path.expanduser("~/.pi/agent/sessions/--Users-sahil-Projects-bhatti--")
files = sorted(glob.glob(os.path.join(SESS_DIR, "*.jsonl")))

def text_of(content):
    out=[]
    if isinstance(content,str): return content
    if isinstance(content,list):
        for p in content:
            if isinstance(p,dict) and p.get('type')=='text':
                out.append(p.get('text',''))
    return "\n".join(out)

sessions=[]
for fp in files:
    fn=os.path.basename(fp)
    size=os.path.getsize(fp)
    user_msgs=[]; n_msg=0; n_tool=0; models=set(); tools=Counter()
    ts=None
    try:
        for line in open(fp, errors='replace'):
            line=line.strip()
            if not line: continue
            try: o=json.loads(line)
            except: continue
            t=o.get('type')
            if t=='session': ts=o.get('timestamp')
            if t=='model_change': models.add(o.get('modelId'))
            if t!='message': continue
            n_msg+=1
            msg=o.get('message',{})
            role=msg.get('role')
            content=msg.get('content')
            if role=='user':
                txt=text_of(content)
                if txt and txt.strip(): user_msgs.append(txt.strip())
            if role=='assistant' and isinstance(content,list):
                for p in content:
                    if isinstance(p,dict) and p.get('type')=='toolCall':
                        n_tool+=1; tools[p.get('name','?')]+=1
    except Exception as e:
        print("ERR",fn,e,file=sys.stderr)
    sessions.append(dict(file=fn, ts=ts, size=size, n_msg=n_msg, n_tool=n_tool,
                         models=sorted(m for m in models if m),
                         tools=dict(tools.most_common()),
                         first_user=user_msgs[0] if user_msgs else "",
                         user_msgs=user_msgs))

json.dump(sessions, open("/Users/sahil/Projects/bhatti-talk/_analysis/index.json","w"), indent=1)
print("sessions:", len(sessions))
print("total size MB:", round(sum(s['size'] for s in sessions)/1e6,1))
print("total messages:", sum(s['n_msg'] for s in sessions))
print("total tool calls:", sum(s['n_tool'] for s in sessions))
print("total user prompts:", sum(len(s['user_msgs']) for s in sessions))
# aggregate tools
allt=Counter()
for s in sessions:
    for k,v in s['tools'].items(): allt[k]+=v
print("\n=== tool usage across all sessions ===")
for k,v in allt.most_common(): print(f"  {v:6d}  {k}")
