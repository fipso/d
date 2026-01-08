# d - Docker Swarm Service Viewer

A terminal UI that combines `docker service ls`, `docker service ps`, and `docker service logs` into a single interactive interface.

Instead of running multiple commands to check your swarm status, `d` gives you a unified view of all services, their tasks, and live logs - all in one place.

## Features

- **Split-pane interface**: Tree view on the left, live logs on the right
- **Two view modes**:
  - **Service view** (default): Groups tasks by service
  - **Node view**: Groups tasks by swarm node
- **Auto-refresh**: Automatically updates data and logs (data: 2s, logs: 1s, fullscreen: 500ms)
- **Real-time logs**: Logs update automatically when selecting a task or service
- **Fullscreen logs**: Press Enter to view logs in fullscreen mode
- **Task-specific logs**: Select a task to see only its logs, select a service to see combined logs
- **Smart task display**: Shows only current/relevant tasks (running or latest failed)
- **Global service support**: Properly handles both replicated and global services
- **Docker context aware**: Respects your current Docker context configuration

## Installation

```bash
go install github.com/youruser/d@latest
```

Or build from source:

```bash
git clone https://github.com/youruser/d
cd d
go build .
```

## Usage

```bash
# View services (default)
d

# View by nodes
d nodes

# Use a specific Docker socket
d --socket /var/run/docker.sock

# Show help
d --help
```

## Keybindings

| Key | Action |
|-----|--------|
| `j` / `↓` | Move cursor down |
| `k` / `↑` | Move cursor up |
| `gg` | Jump to top |
| `G` | Jump to bottom |
| `Enter` | Fullscreen logs mode |
| `yy` | Copy all logs to clipboard (wl-copy) |
| `n` | Toggle between services/nodes view |
| `a` | Toggle auto-refresh |
| `r` | Manual refresh |
| `q` / `Ctrl+C` | Quit |

### Fullscreen Logs Mode

| Key | Action |
|-----|--------|
| `j` / `k` | Scroll logs |
| `gg` / `G` | Jump to top/bottom |
| `yy` | Copy all logs to clipboard |
| `a` | Toggle auto-refresh |
| `q` / `Esc` | Exit fullscreen |

## Views

### Service View (default)

Shows all services with their tasks grouped underneath:

```
▼ my-service (3/3) nginx:latest
  ├─ my-service.1 [running] @node-1
  ├─ my-service.2 [running] @node-2
  └─ my-service.3 [running] @node-3
```

- Select a **service** to see combined logs from all its tasks
- Select a **task** to see only that task's logs

### Node View (`d nodes`)

Shows all swarm nodes with their tasks grouped underneath:

```
▼ node-1 [ready] (5/8 tasks) manager
  ├─ service-a.1 [running] nginx:latest
  ├─ service-b.2 [running] redis:latest
  └─ service-c.1 [failed] myapp:latest
     ↳ task: non-zero exit (1)
```

- Select a **task** to see its logs
- Selecting a **node** shows a prompt to select a task

## Screenshots

<img width="2182" height="1321" alt="image" src="https://github.com/user-attachments/assets/66c02d95-10e3-44c9-818f-f6eb860a71aa" />
<img width="2180" height="1322" alt="image" src="https://github.com/user-attachments/assets/b03f3bf6-edd1-4e01-86d8-0514e2cb71fb" />

## Requirements

- Go 1.21+
- Docker with Swarm mode enabled
- Access to a Docker Swarm manager node
- `wl-copy` for clipboard support (Wayland)

## License

MIT
