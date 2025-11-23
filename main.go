package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

// --- 1. CONFIGURATION ---
const historySeconds = 259200 // 3 Days
const dbFile = "pulse_v30.data.gz"
const confFile = "pulse.conf"

// --- 2. DATA STRUCTURES ---
type AppConfig struct {
	GlobalInt  int      `json:"global_int"`
	ProcessInt int      `json:"process_int"`
	ScriptInt  int      `json:"script_int"`
	CpuWarn    float64  `json:"cpu_warn"`
	CpuCrit    float64  `json:"cpu_crit"`
	MemWarn    float64  `json:"mem_warn"`
	MemCrit    float64  `json:"mem_crit"`
	DskWarn    float64  `json:"dsk_warn"`
	DskCrit    float64  `json:"dsk_crit"`
	SmtpHost   string   `json:"smtp_host"`
	SmtpPort   int      `json:"smtp_port"`
	SmtpUser   string   `json:"smtp_user"`
	SmtpPass   string   `json:"smtp_pass"`
	EmailTo    string   `json:"email_to"`
	Scripts    []string `json:"scripts"`
}

type PluginData struct {
	Path     string  `json:"path"`
	ExitCode int     `json:"exit_code"`
	Output   string  `json:"output"`
	PerfVal  float64 `json:"perf_val"`
	PerfUnit string  `json:"perf_unit"`
}

type PortInfo struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"`
	PID   int32  `json:"pid"`
	Name  string `json:"name"`
}

type ProcessInfo struct {
	PID       int32   `json:"pid"`
	Name      string  `json:"name"`
	CPU       float64 `json:"cpu"`
	Mem       float64 `json:"mem"`
	DiskRead  uint64  `json:"d_read"`
	DiskWrite uint64  `json:"d_write"`
}

type RichMetrics struct {
	Timestamp   int64         `json:"ts"`
	Hostname    string        `json:"host"`
	Uptime      uint64        `json:"uptime"`
	Load1       float64       `json:"load1"`
	Procs       int           `json:"procs"`
	CPUTotal    float64       `json:"cpu_tot"`
	MemUsed     float64       `json:"mem_used"`
	SwapUsed    float64       `json:"swp_used"`
	DiskUsed    float64       `json:"dsk_used"`
	DiskRead    uint64        `json:"dsk_read"`
	DiskWrite   uint64        `json:"dsk_writ"`
	NetDown     uint64        `json:"net_down"`
	NetUp       uint64        `json:"net_up"`
	ProcessList []ProcessInfo `json:"p_list"`
	OpenPorts   []PortInfo    `json:"ports"`
	Plugins     []PluginData  `json:"plugins"`
}

// --- GLOBAL STATE ---
var (
	config    AppConfig
	cfgMutex  sync.RWMutex

	history      []RichMetrics
	historyMutex sync.RWMutex
	
	latestMetric RichMetrics
	latestMutex  sync.RWMutex

	broadcast = make(chan struct{})

	prevNet      net.IOCountersStat
	prevDisk     map[string]disk.IOCountersStat
	prevProcIO   map[int32]process.IOCountersStat
	procCache    map[int32]*process.Process
	initRate     bool = true

	latestProcs   []ProcessInfo
	latestPorts   []PortInfo
	latestPlugins []PluginData
	dataMutex     sync.RWMutex
	procIOMutex   sync.Mutex

	lastEmailTime map[string]time.Time
	alertMutex    sync.Mutex
)

// --- 3. THE DASHBOARD ---
const htmlDashboard = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>Pulse | Enterprise Alerting</title>
    <style>
        :root { --bg: #121212; --card: #1e1e1e; --text: #e0e0e0; --cpu: #00d1b2; --mem: #209cee; --dsk: #ff3860; --net: #ffdd57; --accent: #bd93f9; }
        body { background-color: var(--bg); color: var(--text); font-family: 'Segoe UI', monospace; margin: 0; padding: 15px; box-sizing: border-box; overflow: hidden; }
        * { box-sizing: border-box; }

        .header { display: flex; flex-direction: column; gap: 15px; margin-bottom: 20px; border-bottom: 1px solid #333; padding-bottom: 15px; }
        .top-row { display: flex; justify-content: space-between; align-items: center; }
        .controls-row { display: flex; align-items: center; gap: 10px; background: #1a1a1a; padding: 5px 10px; border-radius: 6px; border: 1px solid #333; flex-wrap: wrap; }

        button { background: #333; border: none; color: #ccc; padding: 5px 10px; cursor: pointer; border-radius: 3px; font-size: 11px; transition: 0.2s; }
        button:hover { background: #555; color: white; }
        button.active { background: var(--cpu); color: #000; font-weight: bold; }
        button.live-btn { background: #ff3860; color: white; font-weight: bold; display: none; }
        input, select { background: #222; border: 1px solid #444; color: #fff; padding: 3px; border-radius: 3px; font-size: 11px; }

        .badge { font-size: 10px; padding: 2px 6px; border-radius: 3px; text-transform: uppercase; font-weight: bold; margin-left: 10px; }
        .badge.live { background: rgba(0, 209, 178, 0.2); color: var(--cpu); border: 1px solid var(--cpu); }
        .badge.hist { background: rgba(255, 221, 87, 0.2); color: var(--net); border: 1px solid var(--net); }

        .grid-main { display: grid; grid-template-columns: 3fr 1fr; gap: 15px; height: calc(100vh - 180px); }
        .col-left { display: flex; flex-direction: column; gap: 15px; overflow-y: auto; padding-right: 5px; padding-bottom: 150px; }
        .col-right { display: flex; flex-direction: column; gap: 15px; overflow-y: auto; height: 100%; padding-bottom: 100px; }

        .card { background: var(--card); border: 1px solid #333; border-radius: 6px; padding: 10px; position: relative; display: flex; flex-direction: column; overflow: hidden; }
        .card-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 5px; height: 20px; flex-shrink: 0; }
        .card-title { font-size: 11px; color: #888; text-transform: uppercase; font-weight: bold; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 70%; }
        .legend { display: flex; gap: 10px; font-size: 10px; }
        .canvas-wrapper { flex: 1; position: relative; min-height: 0; width: 100%; }
        canvas { width: 100%; height: 100%; display: block; }
        
        .zoom-overlay { position: absolute; top: 5px; right: 5px; display: flex; gap: 2px; opacity: 0.3; transition: opacity 0.2s; z-index: 10; }
        .card:hover .zoom-overlay { opacity: 1; }
        .zoom-btn { padding: 2px 6px; font-size: 10px; background: #000; border: 1px solid #444; color: #fff; }

        .drill-controls { display: flex; gap: 10px; margin-bottom: 10px; align-items: center; background: #252525; padding: 10px; border-radius: 4px; flex-shrink: 0; }
        select { background: #111; color: #fff; border: 1px solid #444; padding: 5px; border-radius: 4px; width: 300px; }
        .drill-grid { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 10px; height: 260px; margin-top: 10px; display: none; }
        .drill-grid.active { display: grid; }
        .drill-item { border: 1px solid #333; padding: 5px; border-radius: 4px; display: flex; flex-direction: column; min-width: 0; }

        .modal { display: none; position: fixed; top: 0; left: 0; width: 100%; height: 100%; background: rgba(0,0,0,0.8); z-index: 5000; justify-content: center; align-items: center; }
        .modal-content { background: #1e1e1e; padding: 20px; border-radius: 8px; border: 1px solid #444; width: 600px; max-height: 90vh; overflow-y: auto; }
        .form-group { margin-bottom: 10px; display: flex; justify-content: space-between; align-items: center; }
        .form-group label { font-size: 12px; color: #ccc; }
        .form-group input { width: 60%; }
        .section-title { border-bottom: 1px solid #444; margin: 15px 0 10px 0; font-size: 14px; color: var(--cpu); padding-bottom: 5px; }

        .status-0 { border-left: 3px solid #00d1b2; }
        .status-1 { border-left: 3px solid #ffdd57; } /* Warn */
        .status-2 { border-left: 3px solid #ff3860; } /* Crit */
        .status-3 { border-left: 3px solid #888; }
        .plugin-row { display: flex; justify-content: flex-end; font-size: 10px; margin-left: 10px; color: #fff; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 30%; }

        .table-wrapper { overflow-y: auto; flex: 1; }
        table { width: 100%; border-collapse: collapse; font-size: 10px; }
        th { text-align: left; color: #666; padding: 4px; position: sticky; top: 0; background: var(--card); border-bottom: 1px solid #444; }
        td { padding: 3px 4px; border-bottom: 1px solid #2a2a2a; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 120px; }
        .val-cell { text-align: right; color: #fff; }

        #tooltip { position: absolute; display: none; background: rgba(0,0,0,0.95); padding: 8px; border: 1px solid #555; font-size: 11px; pointer-events: none; z-index: 1000; box-shadow: 0 4px 10px rgba(0,0,0,0.5); white-space: nowrap; }
    </style>
</head>
<body>
    <div id="tooltip"></div>
    
    <div id="settings-modal" class="modal">
        <div class="modal-content">
            <h2 style="margin-top:0;">Configuration</h2>
            <div class="section-title">Custom Monitors (Nagios Scripts)</div>
            <textarea id="in-scripts" style="width:100%; height: 80px; background:#111; color:#ccc; border:1px solid #444; font-family:monospace;" placeholder="e.g. /root/check_disk.sh -w 90 -c 95"></textarea>
            <div class="section-title">Update Rates (Seconds)</div>
            <div class="form-group"><label>Global:</label><input type="number" id="in-int-g"></div>
            <div class="form-group"><label>Process:</label><input type="number" id="in-int-p"></div>
            <div class="form-group"><label>Scripts:</label><input type="number" id="in-int-s"></div>
            <div class="section-title">Alert Thresholds</div>
            <div class="form-group"><label>CPU Warn/Crit:</label><span><input type="number" id="in-cpu-w" style="width:60px"> / <input type="number" id="in-cpu-c" style="width:60px"></span></div>
            <div class="form-group"><label>Mem Warn/Crit:</label><span><input type="number" id="in-mem-w" style="width:60px"> / <input type="number" id="in-mem-c" style="width:60px"></span></div>
            <div class="form-group"><label>Disk Warn/Crit:</label><span><input type="number" id="in-dsk-w" style="width:60px"> / <input type="number" id="in-dsk-c" style="width:60px"></span></div>
            <div class="section-title">Email</div>
            <div class="form-group"><label>Host/Port:</label><span><input type="text" id="in-smtp-host" style="width:100px"> : <input type="number" id="in-smtp-port" style="width:50px"></span></div>
            <div class="form-group"><label>User:</label><input type="text" id="in-smtp-user"></div>
            <div class="form-group"><label>Pass:</label><input type="password" id="in-smtp-pass"></div>
            <div class="form-group"><label>To:</label><input type="text" id="in-email-to"></div>
            <div style="margin-top:20px; text-align:right;">
                <button onclick="closeSettings()">Cancel</button>
                <button onclick="saveSettings()" class="active">Save & Apply</button>
            </div>
        </div>
    </div>

    <div class="header">
        <div class="top-row">
            <h1 style="margin:0; font-size: 20px;">PULSE <span style="color:#666; font-size:0.6em;">// ENTERPRISE</span> <span id="mode-badge" class="badge live">LIVE</span></h1>
            <button onclick="openSettings()" style="margin-left:20px;">⚙️ SETTINGS</button>
        </div>
        <div class="controls-row">
            <span style="font-size:10px; color:#666;">ZOOM:</span>
            <button onclick="zoom(0.3)">+</button> <button onclick="zoom(-0.3)">-</button>
            <button onclick="setLiveDuration(1800)" class="active">30M</button>
            <button onclick="setLiveDuration(86400)">24H</button>
            <div style="width:1px; height:15px; background:#444; margin:0 5px;"></div>
            <input type="datetime-local" id="dp-start">
            <input type="datetime-local" id="dp-end">
            <button onclick="applyRange()">GO</button>
            <button id="btn-live" class="live-btn" onclick="goLive()">RETURN LIVE</button>
        </div>
    </div>

    <div class="grid-main">
        <div class="col-left">
            <div class="card" style="height: 250px; min-height: 250px;">
                <div class="card-header">
                    <div class="card-title">System Resources</div>
                    <div class="legend"><span style="color:#00d1b2">● CPU</span> <span style="color:#209cee">● RAM</span></div>
                </div>
                <div class="canvas-wrapper"><canvas id="c-global"></canvas><div class="zoom-overlay"><button class="zoom-btn" onclick="zoomIn()">+</button><button class="zoom-btn" onclick="zoomOut()">-</button></div></div>
            </div>

            <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 15px; height: 180px; min-height: 180px;">
                <div class="card">
                    <div class="card-header"><div class="card-title">Network</div><div class="legend"><span style="color:#ffdd57">● Rx</span> <span style="color:#bd93f9">● Tx</span></div></div>
                    <div class="canvas-wrapper"><canvas id="c-net"></canvas><div class="zoom-overlay"><button class="zoom-btn" onclick="zoomIn()">+</button><button class="zoom-btn" onclick="zoomOut()">-</button></div></div>
                </div>
                <div class="card">
                    <div class="card-header"><div class="card-title">Disk I/O</div><div class="legend"><span style="color:#ff3860">● Rd</span> <span style="color:#00d1b2">● Wr</span></div></div>
                    <div class="canvas-wrapper"><canvas id="c-disk"></canvas><div class="zoom-overlay"><button class="zoom-btn" onclick="zoomIn()">+</button><button class="zoom-btn" onclick="zoomOut()">-</button></div></div>
                </div>
            </div>

            <div id="plugin-container"></div>

            <div class="card" style="height: auto; min-height: 350px;">
                <div class="card-header"><div class="card-title">Process Inspector</div></div>
                <div style="display:flex; gap:10px; margin-bottom:10px;">
                    <input type="text" id="proc-filter" placeholder="Search..." onkeyup="filterProc()" style="width:100px;">
                    <select id="proc-select" onchange="selProc(this.value)"><option value="">-- Select Process --</option></select>
                </div>
                <div id="drill-view" style="display:grid; grid-template-columns:1fr 1fr 1fr; gap:10px; height:250px; display:none;">
                    <div class="card"><div class="card-title">CPU %</div><div class="canvas-wrapper"><canvas id="c-p-cpu"></canvas></div></div>
                    <div class="card"><div class="card-title">Memory</div><div class="canvas-wrapper"><canvas id="c-p-mem"></canvas></div></div>
                    <div class="card"><div class="card-title">Disk I/O</div><div class="canvas-wrapper"><canvas id="c-p-dsk"></canvas></div></div>
                </div>
            </div>
        </div>

        <div class="col-right">
            <div class="card" style="height: 25%;"><div class="card-title">Top CPU</div><div class="table-wrapper"><table id="tbl-cpu"></table></div></div>
            <div class="card" style="height: 25%;"><div class="card-title">Top Mem</div><div class="table-wrapper"><table id="tbl-mem"></table></div></div>
            <div class="card" style="height: 25%;"><div class="card-title">Top I/O</div><div class="table-wrapper"><table id="tbl-io"></table></div></div>
            <div class="card" style="height: 25%;"><div class="card-title">Ports</div><div class="table-wrapper"><table id="tbl-ports"></table></div></div>
        </div>
    </div>

    <script>
        const STATE = { data: [], mode: 'live', dur: 1800, rStart: 0, rEnd: 0, pid: null, charts: [], plugins: {} };
        const fmtBytes = (v) => { const u=['B','K','M','G']; let i=0; while(v>=1024&&i<3){v/=1024;i++} return v.toFixed(1)+u[i]; }

        function openSettings() {
            fetch('/config').then(r=>r.json()).then(c => {
                const s = (id, val) => document.getElementById(id).value = val || "";
                s("in-cpu-w",c.cpu_warn); s("in-cpu-c",c.cpu_crit); s("in-mem-w",c.mem_warn); s("in-mem-c",c.mem_crit);
                s("in-dsk-w",c.dsk_warn); s("in-dsk-c",c.dsk_crit); s("in-smtp-host",c.smtp_host); s("in-smtp-port",c.smtp_port);
                s("in-smtp-user",c.smtp_user); s("in-smtp-pass",c.smtp_pass); s("in-email-to",c.email_to);
                s("in-int-g",c.global_int); s("in-int-p",c.process_int); s("in-int-s",c.script_int);
                document.getElementById("in-scripts").value = c.scripts ? c.scripts.join("\n") : "";
                document.getElementById("settings-modal").style.display = "flex";
            });
        }
        function closeSettings() { document.getElementById("settings-modal").style.display = "none"; }
        function saveSettings() {
            const g = (id) => document.getElementById(id).value;
            const cfg = {
                cpu_warn: parseFloat(g("in-cpu-w")), cpu_crit: parseFloat(g("in-cpu-c")),
                mem_warn: parseFloat(g("in-mem-w")), mem_crit: parseFloat(g("in-mem-c")),
                dsk_warn: parseFloat(g("in-dsk-w")), dsk_crit: parseFloat(g("in-dsk-c")),
                smtp_host: g("in-smtp-host"), smtp_port: parseInt(g("in-smtp-port")), smtp_user: g("in-smtp-user"), smtp_pass: g("in-smtp-pass"), email_to: g("in-email-to"),
                scripts: g("in-scripts").split("\n").filter(s => s.trim() !== ""),
                global_int: parseInt(g("in-int-g")), process_int: parseInt(g("in-int-p")), script_int: parseInt(g("in-int-s"))
            };
            fetch('/config', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(cfg) })
            .then(() => { closeSettings(); alert("Saved."); });
        }

        class Chart {
            constructor(id, f1, f2, c1, c2, max, unit) {
                this.cvs = document.getElementById(id); this.ctx = this.cvs.getContext("2d");
                this.f1=f1; this.f2=f2; this.c1=c1; this.c2=c2; this.max=max; this.unit=unit;
                STATE.charts.push(this);
                this.cvs.addEventListener('mousemove', e=>this.tip(e));
                this.cvs.addEventListener('mouseleave', ()=>document.getElementById("tooltip").style.display='none');
                this.cvs.addEventListener('wheel', e=>{ if(e.ctrlKey){ e.preventDefault(); zoom(e.deltaY<0?0.2:-0.2); } });
                new ResizeObserver(()=>this.resize()).observe(this.cvs.parentElement);
            }
            resize() { this.cvs.width = this.cvs.parentElement.clientWidth; this.cvs.height = this.cvs.parentElement.clientHeight; this.draw(); }
            draw() {
                const w=this.cvs.width, h=this.cvs.height, pL=40, pB=30;
                this.ctx.clearRect(0,0,w,h);
                if(STATE.data.length<2) return;
                const tEnd = STATE.mode==='live' ? STATE.data[STATE.data.length-1].ts : STATE.rEnd;
                const tStart = STATE.mode==='live' ? tEnd-STATE.dur : STATE.rStart;
                
                const view=[]; for(let d of STATE.data) if(d.ts>=tStart && d.ts<=tEnd) view.push(d);
                if(view.length<2) return;

                let max = this.max || 0;
                if(!this.max) view.forEach(d => max = Math.max(max, this.f1(d), this.f2?this.f2(d):0));
                if(max<=0) max=1; else max*=1.1;

                this.ctx.strokeStyle="#333"; this.ctx.beginPath();
                for(let i=0;i<=5;i++) {
                    let x=pL+(i*(w-pL)/5); this.ctx.moveTo(x,0); this.ctx.lineTo(x,h-pB);
                    let ts=tStart+(i*(tEnd-tStart)/5);
                    this.ctx.fillStyle="#999"; this.ctx.fillText(new Date(ts*1000).toLocaleTimeString(), x-15, h-10);
                }
                for(let i=0;i<=4;i++) {
                    let y=(h-pB)-(i*(h-pB)/4); this.ctx.moveTo(pL,y); this.ctx.lineTo(w,y);
                    let v=i*(max/4); let t=v.toFixed(0);
                    if(this.unit === 'B' || (this.unit === undefined && (this.c1.includes('57') || this.c1.includes('38') || this.c1.includes('20') || (this.c2 && this.c2.includes('00'))))) t=fmtBytes(v);
                    if(this.unit === '%' || this.max === 100) t+='%';
                    this.ctx.fillText(t, 2, y+3);
                }
                this.ctx.stroke();

                const line = (fn, c) => {
                    this.ctx.strokeStyle=c; this.ctx.lineWidth=2; this.ctx.beginPath();
                    view.forEach((d,i) => {
                        let x=pL+((d.ts-tStart)/(tEnd-tStart))*(w-pL);
                        let y=(h-pB)-(fn(d)/max)*(h-pB);
                        if(i===0) this.ctx.moveTo(x,y); else this.ctx.lineTo(x,y);
                    });
                    this.ctx.stroke();
                }
                line(this.f1, this.c1); if(this.f2) line(this.f2, this.c2);
            }
            tip(e) {
                if(STATE.data.length<2) return;
                const rect = this.cvs.getBoundingClientRect();
                const pL=40, w=rect.width;
                const tEnd = STATE.mode==='live' ? STATE.data[STATE.data.length-1].ts : STATE.rEnd;
                const tStart = STATE.mode==='live' ? tEnd-STATE.dur : STATE.rStart;
                const mx = e.clientX - rect.left;
                const mTime = tStart + ((mx-pL)/(w-pL))*(tEnd-tStart);
                const d = STATE.data.reduce((p,c)=> Math.abs(c.ts-mTime)<Math.abs(p.ts-mTime)?c:p);
                const tip = document.getElementById("tooltip");
                tip.style.display="block"; tip.style.left=(e.pageX+15)+"px"; tip.style.top=(e.pageY+15)+"px";
                let h = '<div><b>' + new Date(d.ts*1000).toLocaleTimeString() + '</b></div>';
                let v1 = this.f1(d);
                if(this.unit==='B') v1=fmtBytes(v1); else v1=v1.toFixed(1);
                h += '<div style="color:' + this.c1 + '">V1: ' + v1 + '</div>';
                if(this.f2) {
                    let v2 = this.f2(d);
                    if(this.unit==='B') v2=fmtBytes(v2); else v2=v2.toFixed(1);
                    h += '<div style="color:' + this.c2 + '">V2: ' + v2 + '</div>';
                }
                tip.innerHTML = h;
            }
        }

        new Chart("c-global", d=>d.cpu_tot, d=>d.mem_used, "#00d1b2", "#209cee", 100, "%");
        new Chart("c-net", d=>d.net_down, d=>d.net_up, "#ffdd57", "#bd93f9", null, "B");
        new Chart("c-disk", d=>d.dsk_read, d=>d.dsk_writ, "#ff3860", "#00d1b2", null, "B");
        
        const getP = (d) => { if(!d.p_list) return null; return d.p_list.find(p=>p.pid==STATE.pid); };
        new Chart("c-p-cpu", d=>{const p=getP(d); return p?p.cpu:0}, null, "#00d1b2", null, null, "%");
        new Chart("c-p-mem", d=>{const p=getP(d); return p?p.mem:0}, null, "#209cee", null, null, "B");
        new Chart("c-p-dsk", d=>{const p=getP(d); return p?p.d_read:0}, d=>{const p=getP(d); return p?p.d_write:0}, "#ff3860", "#00d1b2", null, "B");

        function drawAll() { STATE.charts.forEach(c=>c.draw()); }
        function zoom(adj) { STATE.dur = Math.max(60, STATE.dur + (STATE.dur * adj)); STATE.mode='live'; drawAll(); }
        function zoomIn() { zoom(-0.3); } function zoomOut() { zoom(0.3); }
        function setLiveDuration(s) { STATE.mode='live'; STATE.dur=s; drawAll(); }
        function applyRange() { 
            STATE.rStart = new Date(document.getElementById("dp-start").value).getTime()/1000;
            STATE.rEnd = new Date(document.getElementById("dp-end").value).getTime()/1000;
            STATE.mode='range'; drawAll();
        }
        function goLive() { setLiveDuration(1800); }
        function selProc(pid) { 
            STATE.pid = pid; 
            const el = document.getElementById("drill-view");
            if(pid) { el.style.display="grid"; setTimeout(drawAll,50); } else { el.style.display="none"; }
            drawAll(); 
        }
        function filterProc() {
            const f = document.getElementById("proc-filter").value.toUpperCase();
            const opts = document.getElementById("proc-select").options;
            for(let i=1; i<opts.length; i++) opts[i].style.display = opts[i].text.toUpperCase().includes(f) ? "" : "none";
        }

        function updatePlugins(list) {
            const c = document.getElementById("plugin-container");
            if(!list) return;
            const activeIDs = new Set();
            list.forEach(p => {
                let id = "plg-" + btoa(p.path).replace(/[^a-zA-Z0-9]/g, "");
                activeIDs.add(id);
                let card = document.getElementById(id);
                if(!card) {
                    card = document.createElement("div");
                    card.id = id;
                    card.className = "card"; card.style.height="150px"; card.style.marginBottom="15px";
                    card.innerHTML = '<div class="card-header"><div class="card-title">' + p.path + '</div><div id="' + id + '-stat" class="plugin-row"></div></div><div class="canvas-wrapper"><canvas id="' + id + '-cvs"></canvas></div>';
                    c.appendChild(card);
                    new Chart(id+"-cvs", d => {
                        const plug = d.plugins ? d.plugins.find(x=>x.path===p.path) : null;
                        return plug ? plug.perf_val : 0;
                    }, null, "#bd93f9", null, null, p.perf_unit);
                }
                const st = document.getElementById(id+"-stat");
                st.className = "plugin-row status-"+p.exit_code;
                st.innerText = p.output;
            });
            Array.from(c.children).forEach(child => {
                if (!activeIDs.has(child.id)) c.removeChild(child);
            });
        }

        const evt = new EventSource("/events");
        evt.onmessage = (e) => {
            const m = JSON.parse(e.data);
            STATE.data.push(m);
            if(STATE.data.length > 86400) STATE.data.shift();

            if(STATE.mode==='live') updatePlugins(m.plugins);

            if(m.ts % 2 === 0 && m.p_list) {
                const tbl = (id, l, f) => {
                    document.getElementById(id).innerHTML = l.map(p=> '<tr><td>' + p.pid + '</td><td>' + p.name + '</td><td class="val-cell">' + f(p) + '</td></tr>').join("");
                };
                tbl("tbl-cpu", [...m.p_list].sort((a,b)=>b.cpu-a.cpu).slice(0,5), p=>p.cpu.toFixed(1)+"%");
                tbl("tbl-mem", [...m.p_list].sort((a,b)=>b.mem-a.mem).slice(0,5), p=>fmtBytes(p.mem));
                tbl("tbl-io", [...m.p_list].sort((a,b)=>(b.d_read+b.d_write)-(a.d_read+a.d_write)).slice(0,5), p=>fmtBytes(p.d_read+p.d_write)+"/s");
                
                const sel = document.getElementById("proc-select");
                if(document.getElementById("proc-filter").value === "" && (sel.options.length < 2 || m.ts % 10 === 0)) {
                    const val = sel.value;
                    sel.innerHTML = "<option value=''>-- Select --</option>" + [...m.p_list].sort((a,b)=>b.cpu-a.cpu).map(p=> '<option value="' + p.pid + '">' + p.name + '</option>').join("");
                    sel.value = val;
                }
            }
            if(m.ports && m.ts % 5 === 0) {
                document.getElementById("tbl-ports").innerHTML = m.ports.map(p=> '<tr><td>' + p.port + '</td><td>' + p.proto + '</td><td>' + p.name + '</td></tr>').join("");
            }
            if(STATE.mode==='live') drawAll();
        };
        
        fetch("/history").then(r=>r.json()).then(d=>{ if(d) STATE.data=d; drawAll(); });
    </script>
</body>
</html>
`

// --- 4. BACKEND ---

func loadConfig() {
	if _, err := os.Stat(confFile); err == nil {
		f, _ := os.Open(confFile)
		defer f.Close()
		json.NewDecoder(f).Decode(&config)
	}
	if config.GlobalInt == 0 { config.GlobalInt = 2 }
	if config.ProcessInt == 0 { config.ProcessInt = 5 }
	if config.ScriptInt == 0 { config.ScriptInt = 60 }
	lastEmailTime = make(map[string]time.Time)
}

func saveConfig() {
	cfgMutex.Lock(); defer cfgMutex.Unlock()
	cleanScripts := []string{}
	seen := make(map[string]bool)
	for _, s := range config.Scripts {
		trim := strings.TrimSpace(s)
		if trim != "" && !seen[trim] { cleanScripts = append(cleanScripts, trim); seen[trim] = true }
	}
	config.Scripts = cleanScripts
	f, _ := os.Create(confFile); defer f.Close()
	json.NewEncoder(f).Encode(config)
}

func runPlugin(commandLine string) PluginData {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", commandLine)
	} else {
		cmd = exec.Command("sh", "-c", commandLine)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	
	code := 0
	if err != nil { if e, ok := err.(*exec.ExitError); ok { code = e.ExitCode() } else { code = 3 } }

	full := out.String()
	parts := strings.Split(full, "|")
	msg := strings.TrimSpace(parts[0])
	val := 0.0
	unit := ""

	if len(parts) > 1 {
		perf := strings.TrimSpace(parts[1])
		re := regexp.MustCompile(`=([-0-9.]+)([a-zA-Z%]*)`)
		matches := re.FindStringSubmatch(perf)
		if len(matches) >= 2 {
			val, _ = strconv.ParseFloat(matches[1], 64)
			if len(matches) > 2 { unit = matches[2] }
		}
	}
	return PluginData{Path: commandLine, ExitCode: code, Output: msg, PerfVal: val, PerfUnit: unit}
}

func checkAlerts(m RichMetrics) {
	cfgMutex.RLock(); defer cfgMutex.RUnlock()
	// Standard Thresholds
	check := func(n string, v, w, c float64) {
		if w==0 && c==0 { return }
		lvl := ""
		if v >= c { lvl = "CRITICAL" } else if v >= w { lvl = "WARNING" }
		if lvl != "" { sendAlertEmail(n, lvl, v, "") }
	}
	check("CPU", m.CPUTotal, config.CpuWarn, config.CpuCrit)
	check("Memory", m.MemUsed, config.MemWarn, config.MemCrit)
	check("Disk", m.DiskUsed, config.DskWarn, config.DskCrit)

	// Plugin Alerts
	for _, p := range m.Plugins {
		if p.ExitCode == 1 { sendAlertEmail(p.Path, "WARNING", p.PerfVal, p.Output) }
		if p.ExitCode == 2 { sendAlertEmail(p.Path, "CRITICAL", p.PerfVal, p.Output) }
	}
}

func sendAlertEmail(name, level string, val float64, extraMsg string) {
	if config.SmtpHost == "" { return }
	alertMutex.Lock(); defer alertMutex.Unlock()
	
	key := name + level
	if t, ok := lastEmailTime[key]; ok { if time.Since(t) < 15*time.Minute { return } }
	lastEmailTime[key] = time.Now()

	go func() {
		msg := fmt.Sprintf("To: %s\r\nSubject: Pulse Alert: %s %s\r\n\r\nMonitor: %s\nStatus: %s\nValue: %.2f\nMessage: %s\nHost: %s", 
			config.EmailTo, level, name, name, level, val, extraMsg, latestMetric.Hostname)
		
		addr := fmt.Sprintf("%s:%d", config.SmtpHost, config.SmtpPort)
		var err error
		if config.SmtpPort == 465 {
			// Implicit SSL
			tlsConfig := &tls.Config{InsecureSkipVerify: true, ServerName: config.SmtpHost}
			conn, e := tls.Dial("tcp", addr, tlsConfig); if e != nil { return }
			c, e := smtp.NewClient(conn, config.SmtpHost); if e != nil { return }
			auth := smtp.PlainAuth("", config.SmtpUser, config.SmtpPass, config.SmtpHost)
			if err = c.Auth(auth); err == nil {
				if err = c.Mail(config.SmtpUser); err == nil {
					if err = c.Rcpt(config.EmailTo); err == nil {
						w, _ := c.Data(); w.Write([]byte(msg)); w.Close()
					}
				}
			}
			c.Quit()
		} else {
			// STARTTLS
			auth := smtp.PlainAuth("", config.SmtpUser, config.SmtpPass, config.SmtpHost)
			err = smtp.SendMail(addr, auth, config.SmtpUser, []string{config.EmailTo}, []byte(msg))
		}
		if err != nil { fmt.Println("Email Error:", err) }
	}()
}

func startCollector() {
	loadConfig()
	t := time.NewTicker(100 * time.Millisecond); defer t.Stop()
	lG := time.Now(); lP := time.Now(); lS := time.Now()
	for range t.C {
		cfgMutex.RLock()
		gI, pI, sI, sc := config.GlobalInt, config.ProcessInt, config.ScriptInt, config.Scripts
		cfgMutex.RUnlock()
		n := time.Now()
		if n.Sub(lG) >= time.Duration(gI)*time.Second { collectGlobal(); lG = n }
		if n.Sub(lP) >= time.Duration(pI)*time.Second { collectProcesses(); lP = n }
		if n.Sub(lS) >= time.Duration(sI)*time.Second { go collectScripts(sc); lS = n }
	}
}

func collectScripts(s []string) {
	var r []PluginData
	for _, p := range s { r = append(r, runPlugin(p)) }
	dataMutex.Lock(); latestPlugins = r; dataMutex.Unlock()
}

func collectGlobal() {
	hInfo, _ := host.Info(); lAvg, _ := load.Avg(); pids, _ := process.Pids()
	cTot, _ := cpu.Percent(0, false); vMem, _ := mem.VirtualMemory(); sMem, _ := mem.SwapMemory()
	dUsage, _ := disk.Usage("/"); dIO, _ := disk.IOCounters()
	var dR, dW uint64
	for _, io := range dIO { dR += io.ReadBytes; dW += io.WriteBytes }
	nIO, _ := net.IOCounters(false)
	var rx, tx uint64
	if len(nIO) > 0 {
		if !initRate { rx = nIO[0].BytesRecv - prevNet.BytesRecv; tx = nIO[0].BytesSent - prevNet.BytesSent }
		prevNet = nIO[0]; initRate = false
	}
	dataMutex.RLock(); pL := latestProcs; pts := latestPorts; plg := latestPlugins; dataMutex.RUnlock()
	vT := 0.0; if len(cTot)>0 { vT = cTot[0] }
	m := RichMetrics{Timestamp: time.Now().Unix(), Hostname: hInfo.Hostname, Uptime: hInfo.Uptime, Load1: lAvg.Load1, Procs: len(pids), CPUTotal: vT, MemUsed: vMem.UsedPercent, SwapUsed: sMem.UsedPercent, DiskUsed: dUsage.UsedPercent, DiskRead: dR, DiskWrite: dW, NetDown: rx, NetUp: tx, ProcessList: pL, OpenPorts: pts, Plugins: plg}
	checkAlerts(m)
	historyMutex.Lock()
	history = append(history, m)
	if len(history) > historySeconds { history = history[1:] }
	historyMutex.Unlock()
	latestMutex.Lock(); latestMetric = m; latestMutex.Unlock()
	select { case broadcast <- struct{}{}: default: }
}

func collectProcesses() {
	p := getProcessStats(); pts := getPorts()
	dataMutex.Lock(); latestProcs = p; latestPorts = pts; dataMutex.Unlock()
}

func saveHistory() {
	historyMutex.RLock(); defer historyMutex.RUnlock()
	f, _ := os.Create(dbFile); defer f.Close()
	gz := gzip.NewWriter(f); defer gz.Close()
	gob.NewEncoder(gz).Encode(history)
}

func loadHistory() {
	f, err := os.Open(dbFile); if err!=nil { return }; defer f.Close()
	gz, err := gzip.NewReader(f); if err!=nil { return }; defer gz.Close()
	gob.NewDecoder(gz).Decode(&history)
}

func getProcessStats() []ProcessInfo {
	procs, _ := process.Processes(); var list []ProcessInfo
	procIOMutex.Lock(); defer procIOMutex.Unlock()
	if procCache==nil { procCache=make(map[int32]*process.Process) }
	if prevProcIO==nil { prevProcIO=make(map[int32]process.IOCountersStat) }
	seen := make(map[int32]bool)
	for _, p := range procs {
		seen[p.Pid] = true
		if _, ok := procCache[p.Pid]; !ok { procCache[p.Pid] = p }
		proc := procCache[p.Pid]
		c, _ := proc.CPUPercent(); m, _ := proc.MemoryInfo()
		var dR, dW uint64
		io, err := proc.IOCounters()
		if err==nil {
			if pv, ok := prevProcIO[p.Pid]; ok {
				if io.ReadBytes >= pv.ReadBytes { dR = io.ReadBytes - pv.ReadBytes }
				if io.WriteBytes >= pv.WriteBytes { dW = io.WriteBytes - pv.WriteBytes }
			}
			prevProcIO[p.Pid] = *io
		}
		mv := 0.0; if m!=nil { mv = float64(m.RSS) }
		n, _ := proc.Name()
		if c>=0 || mv>1024*1024 { list = append(list, ProcessInfo{PID: p.Pid, Name: n, CPU: c, Mem: mv, DiskRead: dR, DiskWrite: dW}) }
	}
	for pid := range procCache { if !seen[pid] { delete(procCache, pid); delete(prevProcIO, pid) } }
	sort.Slice(list, func(i, j int) bool { return (list[i].CPU + list[i].Mem/1024/1024) > (list[j].CPU + list[j].Mem/1024/1024) })
	if len(list)>500 { return list[:500] }
	return list
}

func getPorts() []PortInfo {
	c, _ := net.Connections("inet"); var res []PortInfo
	for _, x := range c {
		if x.Status == "LISTEN" {
			n := ""; if x.Pid > 0 { if p, err := process.NewProcess(x.Pid); err == nil { n, _ = p.Name() } }
			res = append(res, PortInfo{Port: int(x.Laddr.Port), Proto: getProto(x.Type), PID: x.Pid, Name: n})
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Port < res[j].Port })
	return res
}
func getProto(t uint32) string { if t==1 { return "TCP" }; if t==2 { return "UDP" }; return strconv.Itoa(int(t)) }

func main() {
	history = make([]RichMetrics, 0, historySeconds)
	loadHistory()
	go startCollector()
	c := make(chan os.Signal, 1); signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() { <-c; saveHistory(); os.Exit(0) }()
	go func() { for range time.Tick(1 * time.Minute) { saveHistory() } }()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html"); fmt.Fprint(w, htmlDashboard)
	})
	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var c AppConfig; json.NewDecoder(r.Body).Decode(&c)
			cfgMutex.Lock(); config = c; cfgMutex.Unlock(); saveConfig()
		} else { cfgMutex.RLock(); json.NewEncoder(w).Encode(config); cfgMutex.RUnlock() }
	})
	http.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json"); historyMutex.RLock(); defer historyMutex.RUnlock()
		json.NewEncoder(w).Encode(history)
	})
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream"); w.Header().Set("Cache-Control", "no-cache"); w.Header().Set("Connection", "keep-alive")
		for {
			select {
			case <-r.Context().Done(): return
			case <-broadcast:
				latestMutex.RLock(); d, _ := json.Marshal(latestMetric); latestMutex.RUnlock()
				fmt.Fprintf(w, "data: %s\n\n", d); if f, ok := w.(http.Flusher); ok { f.Flush() }
			}
		}
	})
	fmt.Println("PULSE v30: FULL ALERTING SUITE"); fmt.Println("http://localhost:8080"); http.ListenAndServe(":8080", nil)
}
