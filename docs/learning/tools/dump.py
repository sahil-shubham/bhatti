import json, os, sys, re
SESS_DIR = os.path.expanduser("~/.pi/agent/sessions/--Users-sahil-Projects-bhatti--")
fn=sys.argv[1]
fp=os.path.join(SESS_DIR, fn)
mode = sys.argv[2] if len(sys.argv)>2 else "ut"  # u=user, t=text(assistant), k=thinking

def emit(role, parts):
    print(f"\n{'='*70}\n[{role}]\n{'='*70}")
    print(parts.strip()[:6000])

for line in open(fp, errors='replace'):
    try: o=json.loads(line)
    except: continue
    if o.get('type')!='message': continue
    msg=o.get('message',{}); role=msg.get('role'); content=msg.get('content')
    if role=='user':
        if 'u' not in mode: continue
        txt = content if isinstance(content,str) else "\n".join(p.get('text','') for p in content if isinstance(p,dict) and p.get('type')=='text')
        if txt.strip(): emit('USER', txt)
    elif role=='assistant' and isinstance(content,list):
        buf=[]
        for p in content:
            if not isinstance(p,dict): continue
            if p.get('type')=='thinking' and 'k' in mode: buf.append('[THINKING]\n'+p.get('thinking',''))
            elif p.get('type')=='text' and 't' in mode: buf.append(p.get('text',''))
            elif p.get('type')=='toolCall' and 'c' in mode:
                buf.append(f"[TOOL {p.get('name')}] {json.dumps(p.get('arguments',{}))[:300]}")
        if buf: emit('ASSISTANT', "\n".join(buf))
