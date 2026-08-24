import json, os, glob, sys, re
SESS_DIR = os.path.expanduser("~/.pi/agent/sessions/--Users-sahil-Projects-bhatti--")
files = sorted(glob.glob(os.path.join(SESS_DIR, "*.jsonl")))

def text_of(content):
    if isinstance(content,str): return content
    out=[]
    if isinstance(content,list):
        for p in content:
            if isinstance(p,dict):
                if p.get('type')=='text': out.append(p.get('text',''))
                elif p.get('type')=='thinking': out.append('[THINKING] '+p.get('thinking',''))
    return "\n".join(out)

terms = [t.lower() for t in sys.argv[1:]]
if not terms:
    print("usage: search.py term1 [term2 ...]"); sys.exit(1)

# Count hits per session for the FIRST term (primary), report file + count
hits = {}  # file -> count of matching turns
for fp in files:
    fn=os.path.basename(fp)
    c=0
    try:
        for line in open(fp, errors='replace'):
            if not any(t in line.lower() for t in terms): continue
            try: o=json.loads(line)
            except: continue
            if o.get('type')!='message': continue
            txt=text_of(o.get('message',{}).get('content'))
            if any(t in txt.lower() for t in terms): c+=1
    except: pass
    if c: hits[fn]=c

for fn,c in sorted(hits.items(), key=lambda x:-x[1]):
    print(f"{c:4d}  {fn}")
print(f"\nTOTAL sessions matching: {len(hits)}  TOTAL turns: {sum(hits.values())}")
