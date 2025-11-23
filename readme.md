
***

# 💓 Pulse Agent

**The Single-Binary, Real-Time Infrastructure Monitoring Command Center.**

Pulse is a lightweight, zero-dependency observability agent written in Go. It provides second-by-second visibility into your server's performance, process activity, network traffic, and disk I/O, visualized through a stunning, responsive web dashboard.

![Pulse Dashboard](https://via.placeholder.com/1200x600.png?text=Pulse+Dashboard+Screenshot)

---

## 🚀 Key Features

*   **📦 Single Binary:** No database, no Python/Node dependencies. Just one executable.
*   **⚡ Real-Time Granularity:** Metrics are pushed every second via Server-Sent Events (SSE).
*   **📉 Ultra-Low CPU Usage:** "Singleton Collector" architecture ensures the agent typically uses < 5% CPU, even with multiple browser tabs open.
*   **🕰️ Time-Travel & Persistence:**
    *   Review history with absolute date ranges (e.g., "Yesterday 2 PM - 4 PM").
    *   Data is compressed (GZIP) and saved to disk (`pulse.data.gz`), surviving restarts.
*   **🔍 Process Deep Dive:**
    *   Searchable dropdown of all active processes.
    *   Drill down to view per-process **CPU**, **Memory**, and **Disk I/O** graphs.
*   **🔌 Custom Script Engine (Nagios Compatible):**
    *   Run Bash, Python, PowerShell, or Batch scripts.
    *   Automatically parses performance data (`| label=value`) and graphs it.
    *   Supports alerting on script exit codes.
*   **🔔 Alerting & Email:** Built-in SMTP client to send notifications when thresholds are breached.
*   **🛡️ Network Mapper:** Real-time view of open ports, protocols, and the processes listening on them.
*   **💻 Cross-Platform:** Native support for Linux and Windows.

---

## 🛠️ Installation & Usage

### Prerequisites
*   **Go 1.20+** (Required to build the source).

### 1. Setup Project
Open your terminal (Linux) or PowerShell (Windows) and run:

```bash
# Create directory
mkdir pulse-agent
cd pulse-agent

# Initialize Go module
go mod init pulse

# Download required system monitoring libraries
go get github.com/shirou/gopsutil/v3
```

### 2. Running on Linux 🐧

To monitor all system processes and disk I/O correctly, Pulse should be run with root privileges.

1.  **Save the code:** Save the `main.go` file in the folder.
2.  **Run:**
    ```bash
    sudo go run main.go
    ```
    *Or build a binary:*
    ```bash
    go build -o pulse main.go
    sudo ./pulse
    ```

### 3. Running on Windows 🪟

To access WMI and Performance Counters for all processes, you must run the terminal as **Administrator**.

1.  **Save the code:** Save the `main.go` file in the folder.
2.  **Open PowerShell / CMD:** Right-click the icon and select **"Run as Administrator"**.
3.  **Run:**
    ```powershell
    go run main.go
    ```
    *Or build an executable:*
    ```powershell
    go build -o pulse.exe main.go
    .\pulse.exe
    ```

### 4. Access Dashboard
Open your web browser and navigate to:
👉 **`http://localhost:8080`**

---

## ⚙️ Configuration

Pulse is configured entirely through the **Web UI**. Click the **⚙️ SETTINGS** button in the top header.

### Performance Tuning
*   **Global Interval:** How often CPU/RAM/Net is checked (Default: 2s).
*   **Process Interval:** How often the heavy process list is scanned (Default: 5s).
*   **Script Interval:** How often custom scripts are executed (Default: 60s).

### Alerting & Email
Configure SMTP settings (Host, Port, User, Password) to receive emails.
*   **Debounce:** Emails are rate-limited to once every 15 minutes per alert type to prevent spamming.

### Custom Monitor Scripts (Nagios Style)
Pulse can execute any script and graph the result, provided the script outputs data in the standard Nagios Plugin format.

**Format:**
```text
Status Message Here | 'Label'=Value[Unit];Warn;Crit;Min;Max
```

#### Linux Example (`check_disk.sh`)
```bash
#!/bin/bash
# Save as /root/check_disk.sh and chmod +x
used=$(df / | grep / | awk '{print $5}' | sed 's/%//')
echo "Root Usage: $used% | 'disk_usage'=$used%;85;95;0;100"
exit 0
```
*In Pulse Settings -> Custom Monitors:* `/root/check_disk.sh`

#### Windows Example (`check_ping.bat`)
```batch
@echo off
:: Simple dummy example simulating latency
echo Ping OK: 25ms | 'latency'=25ms;100;500;0;1000
exit /b 0
```
*In Pulse Settings -> Custom Monitors:* `C:\Scripts\check_ping.bat`

---

## 🏗️ Architecture

*   **Backend:** Go (Golang)
    *   **Singleton Collector:** A single goroutine gathers data and broadcasts it to all connected clients, ensuring minimal CPU overhead.
    *   **Exec:** Uses `sh -c` on Linux and `cmd /C` on Windows for script execution.
    *   **Persistence:** Uses `encoding/gob` + `compress/gzip` to save days of history into a small file.
*   **Frontend:** Vanilla JavaScript + HTML5 Canvas
    *   **Zero Frameworks:** No React/Vue/Angular.
    *   **ResizeObserver:** Charts automatically resize and redraw when the window changes or sidebars are toggled.
    *   **EventSource:** Consumes the SSE stream for real-time updates.

---

## 📄 License

Distributed under the MIT License.