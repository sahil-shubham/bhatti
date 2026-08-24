import json, os, sys
SESS_DIR = os.path.expanduser("~/.pi/agent/sessions/--Users-sahil-Projects-bhatti--")
fp=os.path.join(SESS_DIR, sys.argv[1])
n=0
for line in open(fp, errors='replace'):
    try: o=json.loads(line)
    except: continue
    if o.get('type')!='message': continue
    m=o.get('message',{})
    if m.get('role')!='user': continue
    c=m.get('content')
    txt = c if isinstance(c,str) else "\n".join(p.get('text','') for p in c if isinstance(p,dict) and p.get('type')=='text')
    txt=txt.strip()
    if not txt: continue
    n+=1
    print(f"[U{n}] {txt[:280]}")
