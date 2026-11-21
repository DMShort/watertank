#!/usr/bin/env python3
"""Simulated Watershed node (firmware v0.3 protocol) on a pty, for testing the tool."""
import os, pty, sys, time, json, random, math, select, tty
legacy = "--legacy" in sys.argv
m, s = pty.openpty()
tty.setraw(s)
name = os.ttyname(s)
open("/home/claude/sim/port.txt","w").write(name)
print("fake node on", name, flush=True)
cfg = dict(node_id=3, shape="VERT", diam_cm=300.0, len_cm=300.0, width_cm=100.0, empty_cm=200.0, full_cm=30.0)
tx_on = True
true_dist = 125.0
def vol(h):
    c = cfg
    if h <= 0: return 0
    if c["shape"] == "HORIZ":
        r = c["diam_cm"]/2; h = min(h, c["diam_cm"])
        return (r*r*math.acos((r-h)/r) - (r-h)*math.sqrt(2*r*h-h*h)) * c["len_cm"]/1000
    if c["shape"] == "RECT": return c["len_cm"]*c["width_cm"]*h/1000
    r = c["diam_cm"]/2; return math.pi*r*r*h/1000
def cap(): return vol(max(0, cfg["empty_cm"]-cfg["full_cm"]))
def w(line): os.write(m, (line+"\r\n").encode())
def cfgline():
    d = dict(cfg); d["capacity_l"] = round(cap()); d.update(freq_mhz=868.125, sf=7, bw=125.0, cr=7, tx_on=tx_on, radio_ok=True)
    w("@CFG " + json.dumps(d, separators=(",",":")))
def show():
    w(f"config: node_id={cfg['node_id']}  empty_cm={cfg['empty_cm']:.1f}  full_cm={cfg['full_cm']:.1f}")
    cfgline()
def fwver():
    try: return open("/home/claude/sim/fwver.txt").read().strip() or "0.3"
    except OSError: return "0.3"
def boot():
    w("Watershed tank node - bench test")
    if legacy:
        return
    show()
    w('@ID {"fw":"watershed-node","ver":"%s","board":"heltec-v2","node_id":%d}' % (fwver(), cfg["node_id"])); cfgline()
boot()
buf = b""; seq = 0; t0 = time.time(); nxt = time.time() + 2
while True:
    r, _, _ = select.select([m], [], [], 0.1)
    if r:
        try: data = os.read(m, 1024)
        except OSError: break
        buf += data
        while b"\n" in buf:
            line, buf = buf.split(b"\n", 1)
            c = line.decode(errors="ignore").strip().upper()
            if not c or legacy: continue
            if c == "ID":
                w('@ID {"fw":"watershed-node","ver":"%s","board":"heltec-v2","node_id":%d}' % (fwver(), cfg["node_id"])); cfgline()
            elif c == "CAL SHOW": show()
            elif c.startswith("TANK SET SHAPE "):
                v = c[15:]
                if v in ("VERT","HORIZ","RECT"): cfg["shape"] = v; show()
                else: w("@ERR shape must be VERT, HORIZ or RECT")
            elif c.startswith("TANK SET ") or c.startswith("CAL SET "):
                parts = c.split(); val = float(parts[-1]); key = parts[-2]
                k = {"DIAM":"diam_cm","LEN":"len_cm","WIDTH":"width_cm","EMPTY":"empty_cm","FULL":"full_cm"}[key]
                cfg[k] = val; show()
            elif c in ("CAL EMPTY", "CAL FULL"):
                cfg["empty_cm" if c=="CAL EMPTY" else "full_cm"] = round(true_dist,1); show()
            elif c.startswith("NODE SET "): cfg["node_id"] = int(c[9:]); show()
            elif c in ("RADIO TX ON","RADIO TX OFF"): tx_on = c.endswith("ON"); show()
            else: w("@ERR unrecognised command - try HELP")
    if time.time() >= nxt:
        nxt += 2
        if legacy:
            w(f"ds18b20_count=1  distance_cm={true_dist:.1f}  level_pct=40  temp_c=21.4  batt_mv=4950"); w(f"lora_tx seq={seq} bytes=9 result=OK"); seq += 1; continue
        d = None if random.random() < 0.04 else true_dist + random.gauss(0, 0.3)
        h = None if d is None else max(0, min(cfg["empty_cm"]-d, cfg["empty_cm"]-cfg["full_cm"]))
        v = None if h is None else vol(h)
        pct = None if v is None or cap() <= 0 else v/cap()*100
        j = lambda x, n: "null" if x is None else f"{x:.{n}f}"
        w(f"distance_cm={j(d,1)}  height_cm={j(h,1)}")
        w('@R {"seq":%d,"distance_cm":%s,"height_cm":%s,"level_pct":%s,"volume_l":%s,"capacity_l":%.0f,"temp_c":%.2f,"batt_mv":%d,"ds18b20_count":1,"tx":"%s","uptime_s":%d}' %
          (seq, j(d,1), j(h,1), j(pct,1), j(v,0), cap(), 21.3+random.gauss(0,0.05), 4930+random.randint(-20,20), "OK" if tx_on else "OFF", time.time()-t0))
        seq += 1
