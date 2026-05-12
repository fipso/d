package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type ViewMode int

const (
	ViewByService ViewMode = iota
	ViewByNode
)

// Styles
var (
	serviceStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	nodeStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	taskRunningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	taskReadyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	taskStartingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	taskFailedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	taskOtherStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	selectedStyle    = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	errorStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Italic(true)
	dimStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// TreeNode represents a node in the tree view
type TreeNode struct {
	Name        string
	IsParent    bool // Service in service view, Node in node view
	ServiceID   string
	TaskID      string
	ContainerID string
	State       string
	Error       string
	Replicas    string
	Image       string
	Slot        int
	NodeID      string
	NodeName    string
	ServiceName string
	Children    []*TreeNode
}

// Model is the Bubble Tea model
type Model struct {
	nodes      []*TreeNode
	flatList   []flatNode
	cursor     int
	offset     int // Scroll offset
	height     int // Terminal height
	width      int // Terminal width
	err        error
	loading    bool
	viewMode   ViewMode
	lastKey    string // For vim-style gg command

	// Logs panel
	logs           []string // Log lines
	logsOffset     int      // Scroll offset for logs
	logsLoading    bool
	lastSelectedID string // Track which task we're showing logs for

	// Fullscreen logs mode
	fullscreenLogs bool
	logsLastKey    string // For vim-style gg in logs view

	// Line wrapping in logs panel
	lineWrap bool

	// Fullscreen inspect mode
	fullscreenInspect bool
	inspectLoading    bool
	inspectLines      []string
	inspectOffset     int
	inspectTaskID     string
	inspectLastKey    string

	// Auto-refresh
	autoRefresh     bool
	lastLogTime     time.Time // Track last log timestamp for incremental fetching
	lastDataRefresh time.Time // Track last data refresh
}

// Global socket override (set via -s/--socket flag)
var socketOverride string

type flatNode struct {
	node   *TreeNode
	depth  int
	isLast bool
}

// Messages
type dataLoadedMsg struct {
	nodes []*TreeNode
	err   error
}

type dataLoadedSilentMsg struct {
	nodes []*TreeNode
	err   error
}

type logsLoadedMsg struct {
	taskID string
	lines  []string
	err    error
}

type logsAppendedMsg struct {
	taskID string
	lines  []string
	err    error
}

type inspectLoadedMsg struct {
	taskID string
	lines  []string
	err    error
}

type tickMsg time.Time

const (
	tickInterval              = 500 * time.Millisecond
	fullscreenLogRefreshDelay = 500 * time.Millisecond
	normalLogRefreshDelay     = 1 * time.Second
	dataRefreshInterval       = 2 * time.Second
)

// Docker config structures
type dockerConfig struct {
	CurrentContext string `json:"currentContext"`
}

type contextMeta struct {
	Name      string                     `json:"Name"`
	Endpoints map[string]contextEndpoint `json:"Endpoints"`
}

type contextEndpoint struct {
	Host          string `json:"Host"`
	SkipTLSVerify bool   `json:"SkipTLSVerify"`
}

func main() {
	// Parse arguments
	args := os.Args[1:]
	viewMode := ViewByService
	var remainingArgs []string

	// Parse flags
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "-h" || arg == "--help" || arg == "help" {
			printHelp()
			return
		}

		if arg == "-s" || arg == "--socket" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a socket path argument\n", arg)
				os.Exit(1)
			}
			socketOverride = args[i+1]
			i++ // Skip next arg (the socket path)
			continue
		}

		// Handle -s=value or --socket=value
		if strings.HasPrefix(arg, "-s=") {
			socketOverride = strings.TrimPrefix(arg, "-s=")
			continue
		}
		if strings.HasPrefix(arg, "--socket=") {
			socketOverride = strings.TrimPrefix(arg, "--socket=")
			continue
		}

		remainingArgs = append(remainingArgs, arg)
	}

	// Check for subcommand
	if len(remainingArgs) > 0 {
		switch remainingArgs[0] {
		case "nodes":
			viewMode = ViewByNode
		default:
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", remainingArgs[0])
			printHelp()
			os.Exit(1)
		}
	}

	p := tea.NewProgram(initialModel(viewMode), tea.WithAltScreen())
	_, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println(`d - Docker Swarm service viewer

USAGE:
    d [OPTIONS] [COMMAND]

COMMANDS:
    (none)        Show services with their tasks (default)
    nodes         Show nodes with their tasks

OPTIONS:
    -s, --socket PATH    Override Docker socket path (e.g. /var/run/docker.sock)
    -h, --help           Show this help message

KEYBINDINGS:
    j, ↓          Move cursor down
    k, ↑          Move cursor up
    gg            Jump to top
    G             Jump to bottom
    yy            Copy all logs to clipboard (wl-copy)
    Enter         Fullscreen logs (j/k:scroll gg/G:jump q/esc:exit)
    I             Inspect container (hierarchical JSON view, q/esc/I:exit)
    n             Toggle between services/nodes view
    a             Toggle auto-refresh (data:2s, logs:1s, fullscreen logs:500ms)
    r             Refresh
    q, Ctrl+C     Quit

DESCRIPTION:
    Shows Docker Swarm services/tasks in a split view.
    Left panel: tree view of services/tasks
    Right panel: logs for the selected task (auto-updates on selection)

    Default view groups tasks by service.
    'nodes' command groups tasks by swarm node.

    Respects Docker context (set via 'docker context use').`)
}

func initialModel(viewMode ViewMode) Model {
	return Model{
		loading:     true,
		viewMode:    viewMode,
		autoRefresh: true,
	}
}

func (m Model) Init() tea.Cmd {
	if m.autoRefresh {
		return tea.Batch(m.loadData(), tickCmd())
	}
	return m.loadData()
}

func (m Model) loadData() tea.Cmd {
	return func() tea.Msg {
		var nodes []*TreeNode
		var err error
		if m.viewMode == ViewByNode {
			nodes, err = fetchByNode()
		} else {
			nodes, err = fetchByService()
		}
		return dataLoadedMsg{nodes: nodes, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func copyToClipboard(text string) {
	cmd := exec.Command("wl-copy")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}

// getDockerHost resolves the Docker host from context configuration
func getDockerHost() (string, error) {
	// Socket override from -s/--socket flag takes highest priority
	if socketOverride != "" {
		return "unix://" + socketOverride, nil
	}

	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host, nil
	}

	contextName := os.Getenv("DOCKER_CONTEXT")

	if contextName == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil
		}

		configPath := filepath.Join(home, ".docker", "config.json")
		data, err := os.ReadFile(configPath)
		if err != nil {
			return "", nil
		}

		var config dockerConfig
		if err := json.Unmarshal(data, &config); err != nil {
			return "", nil
		}

		contextName = config.CurrentContext
	}

	if contextName == "" || contextName == "default" {
		return "", nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home dir: %w", err)
	}

	hash := sha256.Sum256([]byte(contextName))
	contextDir := hex.EncodeToString(hash[:])

	metaPath := filepath.Join(home, ".docker", "contexts", "meta", contextDir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "", fmt.Errorf("failed to read context %q: %w", contextName, err)
	}

	var meta contextMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("failed to parse context metadata: %w", err)
	}

	endpoint, ok := meta.Endpoints["docker"]
	if !ok {
		return "", fmt.Errorf("no docker endpoint in context %q", contextName)
	}

	return endpoint.Host, nil
}

func newDockerClient() (*client.Client, error) {
	dockerHost, err := getDockerHost()
	if err != nil {
		return nil, err
	}

	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if dockerHost != "" {
		opts = append(opts, client.WithHost(dockerHost))
	} else {
		opts = append(opts, client.FromEnv)
	}

	return client.NewClientWithOpts(opts...)
}

func fetchByService() ([]*TreeNode, error) {
	ctx := context.Background()

	cli, err := newDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	defer cli.Close()

	info, err := cli.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get docker info: %w", err)
	}
	if info.Swarm.LocalNodeState != swarm.LocalNodeStateActive {
		return nil, fmt.Errorf("this node is not part of a swarm")
	}

	services, err := cli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	tasks, err := cli.TaskList(ctx, types.TaskListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}

	nodes, err := cli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	nodeMap := make(map[string]string)
	for _, n := range nodes {
		name := n.Description.Hostname
		if name == "" {
			name = truncateID(n.ID)
		}
		nodeMap[n.ID] = name
	}

	tasksByService := make(map[string][]swarm.Task)
	for _, task := range tasks {
		tasksByService[task.ServiceID] = append(tasksByService[task.ServiceID], task)
	}

	var result []*TreeNode
	for _, svc := range services {
		var replicas string
		if svc.Spec.Mode.Replicated != nil {
			desired := *svc.Spec.Mode.Replicated.Replicas
			running := countRunningTasks(tasksByService[svc.ID])
			replicas = fmt.Sprintf("%d/%d", running, desired)
		} else if svc.Spec.Mode.Global != nil {
			running := countRunningTasks(tasksByService[svc.ID])
			replicas = fmt.Sprintf("%d/global", running)
		}

		image := ""
		if svc.Spec.TaskTemplate.ContainerSpec != nil {
			image = truncateImage(svc.Spec.TaskTemplate.ContainerSpec.Image)
		}

		serviceNode := &TreeNode{
			Name:      svc.Spec.Name,
			IsParent:  true,
			ServiceID: svc.ID,
			Replicas:  replicas,
			Image:     image,
		}

		serviceTasks := tasksByService[svc.ID]
		isGlobal := svc.Spec.Mode.Global != nil

		sort.Slice(serviceTasks, func(i, j int) bool {
			// First, group by slot/node
			if isGlobal {
				if serviceTasks[i].NodeID != serviceTasks[j].NodeID {
					return serviceTasks[i].NodeID < serviceTasks[j].NodeID
				}
			} else {
				if serviceTasks[i].Slot != serviceTasks[j].Slot {
					return serviceTasks[i].Slot < serviceTasks[j].Slot
				}
			}
			// Within same slot/node, running tasks come first
			iRunning := serviceTasks[i].Status.State == swarm.TaskStateRunning
			jRunning := serviceTasks[j].Status.State == swarm.TaskStateRunning
			if iRunning != jRunning {
				return iRunning
			}
			// Then by most recent
			return serviceTasks[i].CreatedAt.After(serviceTasks[j].CreatedAt)
		})

		// For each slot/node, show only ONE task:
		// - Running task if exists
		// - Otherwise the most recent task (already sorted by CreatedAt desc)
		seenKeys := make(map[string]bool)
		for _, task := range serviceTasks {
			var key string
			if isGlobal {
				key = task.NodeID
			} else {
				key = fmt.Sprintf("%d", task.Slot)
			}

			// Skip if we've already added a task for this slot/node
			if seenKeys[key] {
				continue
			}
			seenKeys[key] = true

			nodeName := nodeMap[task.NodeID]
			if nodeName == "" && task.NodeID != "" {
				nodeName = truncateID(task.NodeID)
			}

			containerID := ""
			if task.Status.ContainerStatus != nil {
				containerID = task.Status.ContainerStatus.ContainerID
			}

			// For global services, use node name in task name
			taskName := fmt.Sprintf("%s.%d", svc.Spec.Name, task.Slot)
			if isGlobal {
				taskName = fmt.Sprintf("%s.%s", svc.Spec.Name, nodeName)
			}

			taskNode := &TreeNode{
				Name:        taskName,
				IsParent:    false,
				ServiceID:   svc.ID,
				ServiceName: svc.Spec.Name,
				TaskID:      task.ID,
				ContainerID: containerID,
				State:       string(task.Status.State),
				Error:       task.Status.Err,
				Slot:        int(task.Slot),
				NodeID:      task.NodeID,
				NodeName:    nodeName,
			}
			serviceNode.Children = append(serviceNode.Children, taskNode)
		}

		result = append(result, serviceNode)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

func fetchByNode() ([]*TreeNode, error) {
	ctx := context.Background()

	cli, err := newDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	defer cli.Close()

	info, err := cli.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get docker info: %w", err)
	}
	if info.Swarm.LocalNodeState != swarm.LocalNodeStateActive {
		return nil, fmt.Errorf("this node is not part of a swarm")
	}

	services, err := cli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	serviceMap := make(map[string]swarm.Service)
	for _, svc := range services {
		serviceMap[svc.ID] = svc
	}

	tasks, err := cli.TaskList(ctx, types.TaskListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}

	nodes, err := cli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Build node info map
	type nodeInfo struct {
		ID       string
		Hostname string
		State    swarm.NodeState
		Role     swarm.NodeRole
	}
	nodeInfoMap := make(map[string]nodeInfo)
	for _, n := range nodes {
		hostname := n.Description.Hostname
		if hostname == "" {
			hostname = truncateID(n.ID)
		}
		nodeInfoMap[n.ID] = nodeInfo{
			ID:       n.ID,
			Hostname: hostname,
			State:    n.Status.State,
			Role:     n.Spec.Role,
		}
	}

	// Group tasks by node
	tasksByNode := make(map[string][]swarm.Task)
	for _, task := range tasks {
		if task.NodeID != "" {
			tasksByNode[task.NodeID] = append(tasksByNode[task.NodeID], task)
		}
	}

	var result []*TreeNode
	for _, n := range nodes {
		ni := nodeInfoMap[n.ID]
		taskCount := len(tasksByNode[n.ID])
		runningCount := countRunningTasks(tasksByNode[n.ID])

		roleStr := "worker"
		if ni.Role == swarm.NodeRoleManager {
			roleStr = "manager"
		}

		swarmNode := &TreeNode{
			Name:     ni.Hostname,
			IsParent: true,
			NodeID:   n.ID,
			NodeName: ni.Hostname,
			Replicas: fmt.Sprintf("%d/%d tasks", runningCount, taskCount),
			State:    string(ni.State),
			Image:    roleStr,
		}

		// Filter out tasks whose service no longer exists
		nodeTasks := tasksByNode[n.ID]
		var validTasks []swarm.Task
		for _, task := range nodeTasks {
			if _, ok := serviceMap[task.ServiceID]; ok {
				validTasks = append(validTasks, task)
			}
		}

		// Sort tasks by service name, slot, then newest first
		sort.Slice(validTasks, func(i, j int) bool {
			svcI := serviceMap[validTasks[i].ServiceID]
			svcJ := serviceMap[validTasks[j].ServiceID]
			if svcI.Spec.Name != svcJ.Spec.Name {
				return svcI.Spec.Name < svcJ.Spec.Name
			}
			if validTasks[i].Slot != validTasks[j].Slot {
				return validTasks[i].Slot < validTasks[j].Slot
			}
			return validTasks[i].CreatedAt.After(validTasks[j].CreatedAt)
		})

		// Filter: show only running task OR most recent task per service+slot
		type slotKey struct {
			serviceID string
			slot      int
		}
		seenSlots := make(map[slotKey]bool)

		for _, task := range validTasks {
			key := slotKey{task.ServiceID, int(task.Slot)}
			if seenSlots[key] {
				continue // Already have a task for this slot
			}
			seenSlots[key] = true

			svc := serviceMap[task.ServiceID]
			containerID := ""
			if task.Status.ContainerStatus != nil {
				containerID = task.Status.ContainerStatus.ContainerID
			}

			image := ""
			if svc.Spec.TaskTemplate.ContainerSpec != nil {
				image = truncateImage(svc.Spec.TaskTemplate.ContainerSpec.Image)
			}

			taskNode := &TreeNode{
				Name:        fmt.Sprintf("%s.%d", svc.Spec.Name, task.Slot),
				IsParent:    false,
				ServiceID:   task.ServiceID,
				ServiceName: svc.Spec.Name,
				TaskID:      task.ID,
				ContainerID: containerID,
				State:       string(task.Status.State),
				Error:       task.Status.Err,
				Slot:        int(task.Slot),
				NodeID:      task.NodeID,
				NodeName:    ni.Hostname,
				Image:       image,
			}
			swarmNode.Children = append(swarmNode.Children, taskNode)
		}

		result = append(result, swarmNode)
	}

	// Sort nodes by hostname
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

func countRunningTasks(tasks []swarm.Task) int {
	count := 0
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateRunning {
			count++
		}
	}
	return count
}

func truncateID(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

func truncateImage(image string) string {
	// Remove @sha256:... digest
	if idx := strings.Index(image, "@sha256:"); idx != -1 {
		image = image[:idx]
	}
	// Strip registry path, keep only image name and tag
	if idx := strings.LastIndex(image, "/"); idx != -1 {
		image = image[idx+1:]
	}
	if len(image) > 40 {
		return image[:37] + "..."
	}
	return image
}

func (m *Model) buildFlatList() {
	m.flatList = nil
	for _, node := range m.nodes {
		m.flatList = append(m.flatList, flatNode{node: node, depth: 0, isLast: false})
		for i, child := range node.Children {
			isLast := i == len(node.Children)-1
			m.flatList = append(m.flatList, flatNode{node: child, depth: 1, isLast: isLast})
		}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		m.fixOffset()
		return m, nil

	case dataLoadedMsg:
		m.loading = false
		m.err = msg.err
		m.nodes = msg.nodes
		m.buildFlatList()
		m.cursor = 0
		m.offset = 0
		// Load logs for first selected item
		cmd := m.maybeLoadLogs()
		return m, cmd

	case dataLoadedSilentMsg:
		// Silent refresh - update data but preserve cursor position
		if msg.err == nil {
			oldSelectedID := m.lastSelectedID
			oldCursor := m.cursor
			m.nodes = msg.nodes
			m.buildFlatList()
			// Try to preserve cursor position
			if oldCursor < len(m.flatList) {
				m.cursor = oldCursor
			} else if len(m.flatList) > 0 {
				m.cursor = len(m.flatList) - 1
			}
			m.fixOffset()
			// Preserve lastSelectedID so we don't trigger a log reload
			m.lastSelectedID = oldSelectedID
		}
		return m, nil

	case logsLoadedMsg:
		// Only update if this is for the currently selected task
		if msg.taskID == m.lastSelectedID {
			m.logsLoading = false
			if msg.err != nil {
				m.logs = []string{fmt.Sprintf("Error: %v", msg.err)}
			} else {
				m.logs = msg.lines
			}
			m.lastLogTime = time.Now()
			// Start at the bottom of logs (most recent)
			m.logsOffset = len(m.logs) - (m.height - 3)
			if m.logsOffset < 0 {
				m.logsOffset = 0
			}
		}
		return m, nil

	case inspectLoadedMsg:
		if msg.taskID == m.inspectTaskID {
			m.inspectLoading = false
			if msg.err != nil {
				m.inspectLines = []string{errorStyle.Render("Error: " + msg.err.Error())}
			} else {
				m.inspectLines = msg.lines
			}
			m.inspectOffset = 0
		}
		return m, nil

	case logsAppendedMsg:
		// Append new logs if this is for the currently selected task
		if msg.taskID == m.lastSelectedID {
			if msg.err == nil && len(msg.lines) > 0 {
				// Check if we were at the bottom before appending
				visibleLines := m.height - 3
				if visibleLines < 1 {
					visibleLines = 1
				}
				wasAtBottom := m.logsOffset >= len(m.logs)-visibleLines

				m.logs = append(m.logs, msg.lines...)
				m.lastLogTime = time.Now()

				// Auto-scroll to bottom if we were already there
				if wasAtBottom {
					m.logsOffset = len(m.logs) - visibleLines
					if m.logsOffset < 0 {
						m.logsOffset = 0
					}
				}
			}
		}
		return m, nil

	case tickMsg:
		if !m.autoRefresh {
			return m, nil
		}

		var cmds []tea.Cmd
		cmds = append(cmds, tickCmd()) // Schedule next tick

		now := time.Now()

		// Refresh data periodically
		if now.Sub(m.lastDataRefresh) >= dataRefreshInterval {
			m.lastDataRefresh = now
			cmds = append(cmds, m.loadDataSilent())
		}

		// Refresh logs incrementally with different delays for fullscreen vs normal
		logDelay := normalLogRefreshDelay
		if m.fullscreenLogs {
			logDelay = fullscreenLogRefreshDelay
		}
		if m.lastSelectedID != "" && !m.logsLoading && now.Sub(m.lastLogTime) >= logDelay {
			cmds = append(cmds, m.loadLogsIncremental())
		}

		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		// Handle fullscreen inspect mode
		if m.fullscreenInspect {
			return m.handleFullscreenInspectKey(msg)
		}
		// Handle fullscreen logs mode
		if m.fullscreenLogs {
			return m.handleFullscreenLogsKey(msg)
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "up", "k":
			m.lastKey = ""
			if m.cursor > 0 {
				m.cursor--
				m.fixOffset()
				cmd := m.maybeLoadLogs()
				return m, cmd
			}
			return m, nil

		case "down", "j":
			m.lastKey = ""
			if m.cursor < len(m.flatList)-1 {
				m.cursor++
				m.fixOffset()
				cmd := m.maybeLoadLogs()
				return m, cmd
			}
			return m, nil

		case "enter":
			m.lastKey = ""
			// Enter fullscreen logs mode
			if len(m.logs) > 0 {
				m.fullscreenLogs = true
			}
			return m, nil

		case "g":
			if m.lastKey == "g" {
				// gg - jump to top of tree
				m.cursor = 0
				m.offset = 0
				m.lastKey = ""
				cmd := m.maybeLoadLogs()
				return m, cmd
			}
			m.lastKey = "g"
			return m, nil

		case "G":
			// G - jump to bottom of tree
			m.lastKey = ""
			if len(m.flatList) > 0 {
				m.cursor = len(m.flatList) - 1
				m.fixOffset()
				cmd := m.maybeLoadLogs()
				return m, cmd
			}
			return m, nil

		case "r":
			m.lastKey = ""
			m.loading = true
			return m, m.loadData()

		case "a":
			m.lastKey = ""
			m.autoRefresh = !m.autoRefresh
			if m.autoRefresh {
				m.lastDataRefresh = time.Now()
				m.lastLogTime = time.Now()
				return m, tickCmd()
			}
			return m, nil

		case "n":
			m.lastKey = ""
			// Toggle between service and node view
			if m.viewMode == ViewByService {
				m.viewMode = ViewByNode
			} else {
				m.viewMode = ViewByService
			}
			m.loading = true
			m.lastSelectedID = ""
			m.logs = nil
			return m, m.loadData()

		case "W":
			m.lastKey = ""
			m.lineWrap = !m.lineWrap
			return m, nil

		case "I":
			m.lastKey = ""
			if m.cursor >= len(m.flatList) {
				return m, nil
			}
			node := m.flatList[m.cursor].node
			if node == nil || node.IsParent || node.ContainerID == "" {
				return m, nil
			}
			m.fullscreenInspect = true
			m.inspectLoading = true
			m.inspectLines = nil
			m.inspectOffset = 0
			m.inspectTaskID = node.TaskID
			return m, m.loadInspect(node)

		case "y":
			if m.lastKey == "y" {
				// yy - copy all logs to clipboard
				m.lastKey = ""
				if len(m.logs) > 0 {
					copyToClipboard(strings.Join(m.logs, "\n"))
				}
				return m, nil
			}
			m.lastKey = "y"
			return m, nil
		}
	}

	return m, nil
}

func (m Model) handleFullscreenLogsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	visibleLines := m.height - 3
	if visibleLines < 1 {
		visibleLines = 1
	}

	switch msg.String() {
	case "q", "esc":
		m.fullscreenLogs = false
		m.logsLastKey = ""
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "up", "k":
		m.logsLastKey = ""
		if m.logsOffset > 0 {
			m.logsOffset--
		}
		return m, nil

	case "down", "j":
		m.logsLastKey = ""
		maxOffset := len(m.logs) - visibleLines
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.logsOffset < maxOffset {
			m.logsOffset++
		}
		return m, nil

	case "g":
		if m.logsLastKey == "g" {
			// gg - jump to top of logs
			m.logsOffset = 0
			m.logsLastKey = ""
			return m, nil
		}
		m.logsLastKey = "g"
		return m, nil

	case "G":
		// G - jump to bottom of logs
		m.logsLastKey = ""
		maxOffset := len(m.logs) - visibleLines
		if maxOffset < 0 {
			maxOffset = 0
		}
		m.logsOffset = maxOffset
		return m, nil

	case "a":
		m.logsLastKey = ""
		m.autoRefresh = !m.autoRefresh
		if m.autoRefresh {
			m.lastDataRefresh = time.Now()
			m.lastLogTime = time.Now()
			return m, tickCmd()
		}
		return m, nil

	case "W":
		m.logsLastKey = ""
		m.lineWrap = !m.lineWrap
		return m, nil

	case "y":
		if m.logsLastKey == "y" {
			// yy - copy all logs to clipboard
			m.logsLastKey = ""
			if len(m.logs) > 0 {
				copyToClipboard(strings.Join(m.logs, "\n"))
			}
			return m, nil
		}
		m.logsLastKey = "y"
		return m, nil
	}

	m.logsLastKey = ""
	return m, nil
}

func (m Model) handleFullscreenInspectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	visibleLines := m.height - 3
	if visibleLines < 1 {
		visibleLines = 1
	}

	switch msg.String() {
	case "q", "esc", "I":
		m.fullscreenInspect = false
		m.inspectLastKey = ""
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "up", "k":
		m.inspectLastKey = ""
		if m.inspectOffset > 0 {
			m.inspectOffset--
		}
		return m, nil

	case "down", "j":
		m.inspectLastKey = ""
		maxOffset := len(m.inspectLines) - visibleLines
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.inspectOffset < maxOffset {
			m.inspectOffset++
		}
		return m, nil

	case "g":
		if m.inspectLastKey == "g" {
			m.inspectOffset = 0
			m.inspectLastKey = ""
			return m, nil
		}
		m.inspectLastKey = "g"
		return m, nil

	case "G":
		m.inspectLastKey = ""
		maxOffset := len(m.inspectLines) - visibleLines
		if maxOffset < 0 {
			maxOffset = 0
		}
		m.inspectOffset = maxOffset
		return m, nil

	case "W":
		m.inspectLastKey = ""
		m.lineWrap = !m.lineWrap
		return m, nil

	case "y":
		if m.inspectLastKey == "y" {
			m.inspectLastKey = ""
			if len(m.inspectLines) > 0 {
				copyToClipboard(strings.Join(m.inspectLines, "\n"))
			}
			return m, nil
		}
		m.inspectLastKey = "y"
		return m, nil
	}

	m.inspectLastKey = ""
	return m, nil
}

func (m *Model) maybeLoadLogs() tea.Cmd {
	if m.cursor >= len(m.flatList) {
		return nil
	}
	node := m.flatList[m.cursor].node
	if node == nil {
		return nil
	}

	// In nodes view, parent nodes are swarm nodes (no logs)
	// In service view, parent nodes are services (have logs)
	if node.IsParent && m.viewMode == ViewByNode {
		m.lastSelectedID = ""
		m.logs = []string{"(select a task to view logs)"}
		m.logsOffset = 0
		return nil
	}

	// Use node name as ID for both services and tasks
	nodeID := node.Name
	if !node.IsParent && node.TaskID != "" {
		nodeID = node.TaskID
	}

	if nodeID != "" && nodeID != m.lastSelectedID {
		m.lastSelectedID = nodeID
		m.logsLoading = true
		m.logs = nil
		m.logsOffset = 0
		return m.loadLogs(node)
	}
	return nil
}

func (m *Model) fixOffset() {
	// 2 lines for header, 1 less for terminal
	visibleLines := m.height - 3
	if visibleLines < 1 {
		visibleLines = 1
	}

	// Scroll up if cursor is above visible area
	if m.cursor < m.offset {
		m.offset = m.cursor
	}

	// Scroll down if cursor is below visible area
	// Count actual rendered lines from offset to cursor
	linesUsed := 0
	for i := m.offset; i <= m.cursor && i < len(m.flatList); i++ {
		linesUsed += m.itemHeight(i)
	}
	for linesUsed > visibleLines && m.offset < m.cursor {
		linesUsed -= m.itemHeight(m.offset)
		m.offset++
	}

	// Ensure offset is never negative
	if m.offset < 0 {
		m.offset = 0
	}
}

// itemHeight returns how many lines an item takes when rendered
func (m *Model) itemHeight(index int) int {
	if index < 0 || index >= len(m.flatList) {
		return 1
	}
	node := m.flatList[index].node
	if node.Error != "" {
		return 2 // task line + error line
	}
	return 1
}


func (m Model) loadLogs(node *TreeNode) tea.Cmd {
	if node == nil {
		return nil
	}
	// ID used to match response with current selection
	nodeID := node.Name
	if !node.IsParent && node.TaskID != "" {
		nodeID = node.TaskID
	}

	isTask := !node.IsParent && node.TaskID != ""
	taskID := node.TaskID
	serviceName := node.Name // for parents, Name is the service name
	nodeError := node.Error

	return func() tea.Msg {
		ctx := context.Background()

		cli, err := newDockerClient()
		if err != nil {
			return logsLoadedMsg{taskID: nodeID, err: err}
		}
		defer cli.Close()

		opts := container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Timestamps: true,
			Tail:       "10000",
		}

		var reader io.ReadCloser
		if isTask {
			// Use TaskLogs for specific task
			reader, err = cli.TaskLogs(ctx, taskID, opts)
		} else {
			// Use ServiceLogs for all tasks in service
			reader, err = cli.ServiceLogs(ctx, serviceName, opts)
		}
		if err != nil {
			if nodeError != "" {
				parts := strings.Split(nodeError, ": ")
				errLines := []string{"Error:"}
				for _, part := range parts {
					errLines = append(errLines, "  "+part)
				}
				return logsLoadedMsg{taskID: nodeID, lines: errLines}
			}
			return logsLoadedMsg{taskID: nodeID, err: err}
		}
		defer reader.Close()

		// Read all data first
		data, err := io.ReadAll(reader)
		if err != nil {
			if nodeError != "" {
				parts := strings.Split(nodeError, ": ")
				errLines := []string{"Error:"}
				for _, part := range parts {
					errLines = append(errLines, "  "+part)
				}
				return logsLoadedMsg{taskID: nodeID, lines: errLines}
			}
			return logsLoadedMsg{taskID: nodeID, err: err}
		}

		// Demultiplex docker log stream - write both stdout and stderr to same
		// destination to preserve chronological order
		var combined strings.Builder
		_, copyErr := stdcopy.StdCopy(&combined, &combined, strings.NewReader(string(data)))

		var rawLines []string
		if copyErr != nil {
			// Fallback: treat as raw stream (TTY containers or raw logs)
			rawLines = strings.Split(string(data), "\n")
		} else {
			rawLines = strings.Split(combined.String(), "\n")
		}

		// Filter out empty trailing lines
		var lines []string
		for _, line := range rawLines {
			lines = append(lines, line)
		}
		// Remove trailing empty lines
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) == 0 {
			lines = []string{"(no logs)"}
		}
		if nodeError != "" {
			// Split chained error on ": " so it doesn't get truncated
			parts := strings.Split(nodeError, ": ")
			errLines := []string{"Error:"}
			for _, part := range parts {
				errLines = append(errLines, "  "+part)
			}
			errLines = append(errLines, "")
			lines = append(errLines, lines...)
		}

		return logsLoadedMsg{taskID: nodeID, lines: lines}
	}
}

// loadDataSilent refreshes data without showing loading state
func (m Model) loadDataSilent() tea.Cmd {
	return func() tea.Msg {
		var nodes []*TreeNode
		var err error
		if m.viewMode == ViewByNode {
			nodes, err = fetchByNode()
		} else {
			nodes, err = fetchByService()
		}
		return dataLoadedSilentMsg{nodes: nodes, err: err}
	}
}

// loadLogsIncremental fetches only new logs since last fetch
func (m Model) loadLogsIncremental() tea.Cmd {
	if m.cursor >= len(m.flatList) {
		return nil
	}
	node := m.flatList[m.cursor].node
	if node == nil {
		return nil
	}

	// In nodes view, parent nodes are swarm nodes (no logs)
	if node.IsParent && m.viewMode == ViewByNode {
		return nil
	}

	nodeID := node.Name
	if !node.IsParent && node.TaskID != "" {
		nodeID = node.TaskID
	}

	isTask := !node.IsParent && node.TaskID != ""
	taskID := node.TaskID
	serviceName := node.Name
	since := m.lastLogTime

	return func() tea.Msg {
		ctx := context.Background()

		cli, err := newDockerClient()
		if err != nil {
			return logsAppendedMsg{taskID: nodeID, err: err}
		}
		defer cli.Close()

		opts := container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Timestamps: true,
			Since:      since.Format(time.RFC3339Nano),
		}

		var reader io.ReadCloser
		if isTask {
			reader, err = cli.TaskLogs(ctx, taskID, opts)
		} else {
			reader, err = cli.ServiceLogs(ctx, serviceName, opts)
		}
		if err != nil {
			return logsAppendedMsg{taskID: nodeID, err: err}
		}
		defer reader.Close()

		data, err := io.ReadAll(reader)
		if err != nil {
			return logsAppendedMsg{taskID: nodeID, err: err}
		}

		if len(data) == 0 {
			return logsAppendedMsg{taskID: nodeID, lines: nil}
		}

		var combined strings.Builder
		_, copyErr := stdcopy.StdCopy(&combined, &combined, strings.NewReader(string(data)))

		var rawLines []string
		if copyErr != nil {
			rawLines = strings.Split(string(data), "\n")
		} else {
			rawLines = strings.Split(combined.String(), "\n")
		}

		// Filter empty lines
		var lines []string
		for _, line := range rawLines {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}

		return logsAppendedMsg{taskID: nodeID, lines: lines}
	}
}

func (m Model) loadInspect(node *TreeNode) tea.Cmd {
	taskID := node.TaskID
	containerID := node.ContainerID
	return func() tea.Msg {
		ctx := context.Background()

		cli, err := newDockerClient()
		if err != nil {
			return inspectLoadedMsg{taskID: taskID, err: err}
		}
		defer cli.Close()

		inspect, err := cli.ContainerInspect(ctx, containerID)
		if err != nil {
			return inspectLoadedMsg{taskID: taskID, err: err}
		}

		raw, err := json.Marshal(inspect)
		if err != nil {
			return inspectLoadedMsg{taskID: taskID, err: err}
		}
		var tree map[string]interface{}
		if err := json.Unmarshal(raw, &tree); err != nil {
			return inspectLoadedMsg{taskID: taskID, err: err}
		}

		return inspectLoadedMsg{taskID: taskID, lines: renderInspectTree(tree)}
	}
}

var (
	inspectKeyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	inspectMarkerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	inspectNullStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	inspectBoolStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	inspectNumStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
)

func renderInspectTree(v interface{}) []string {
	var out []string
	if m, ok := v.(map[string]interface{}); ok {
		keys := sortedKeys(m)
		for _, k := range keys {
			renderInspectField(&out, "", k, m[k])
		}
	} else {
		renderInspectField(&out, "", "", v)
	}
	return out
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func renderInspectField(out *[]string, indent, key string, v interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		if len(val) == 0 {
			*out = append(*out, fmt.Sprintf("%s%s %s",
				indent,
				inspectKeyStyle.Render(key+":"),
				inspectMarkerStyle.Render("{}")))
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s %s",
			indent,
			inspectMarkerStyle.Render("▼"),
			inspectKeyStyle.Render(key)))
		childIndent := indent + "  "
		for _, k := range sortedKeys(val) {
			renderInspectField(out, childIndent, k, val[k])
		}

	case []interface{}:
		if len(val) == 0 {
			*out = append(*out, fmt.Sprintf("%s%s %s",
				indent,
				inspectKeyStyle.Render(key+":"),
				inspectMarkerStyle.Render("[]")))
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s",
			indent,
			inspectKeyStyle.Render(key)))
		itemIndent := indent + "  "
		for i, item := range val {
			renderInspectArrayItem(out, itemIndent, i, item)
		}

	default:
		*out = append(*out, fmt.Sprintf("%s%s %s",
			indent,
			inspectKeyStyle.Render(key+":"),
			formatInspectScalar(v)))
	}
}

func renderInspectArrayItem(out *[]string, indent string, idx int, v interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		if len(val) == 0 {
			*out = append(*out, fmt.Sprintf("%s%s %s",
				indent,
				inspectMarkerStyle.Render("-"),
				inspectMarkerStyle.Render("{}")))
			return
		}
		*out = append(*out, fmt.Sprintf("%s%s %s",
			indent,
			inspectMarkerStyle.Render("▼"),
			inspectKeyStyle.Render(fmt.Sprintf("[%d]", idx))))
		childIndent := indent + "  "
		for _, k := range sortedKeys(val) {
			renderInspectField(out, childIndent, k, val[k])
		}
	case []interface{}:
		// Nested array; render header then recurse items
		*out = append(*out, fmt.Sprintf("%s%s %s",
			indent,
			inspectMarkerStyle.Render("-"),
			inspectKeyStyle.Render(fmt.Sprintf("[%d]", idx))))
		itemIndent := indent + "  "
		for i, item := range val {
			renderInspectArrayItem(out, itemIndent, i, item)
		}
	default:
		*out = append(*out, fmt.Sprintf("%s%s %s",
			indent,
			inspectMarkerStyle.Render("-"),
			formatInspectScalar(v)))
	}
}

func formatInspectScalar(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return inspectNullStyle.Render("null")
	case bool:
		if val {
			return inspectBoolStyle.Render("true")
		}
		return inspectBoolStyle.Render("false")
	case float64:
		// JSON numbers come back as float64; print as integer when possible
		if val == float64(int64(val)) {
			return inspectNumStyle.Render(fmt.Sprintf("%d", int64(val)))
		}
		return inspectNumStyle.Render(fmt.Sprintf("%g", val))
	case string:
		if val == "" {
			return inspectNullStyle.Render(`""`)
		}
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}

func (m Model) View() string {
	if m.loading {
		return "Loading...\n"
	}

	if m.err != nil {
		return fmt.Sprintf("Error: %v\n\nPress 'q' to quit, 'r' to retry.\n", m.err)
	}

	if len(m.nodes) == 0 {
		return "No data found.\n\nPress 'q' to quit, 'r' to refresh.\n"
	}

	// Fullscreen inspect view
	if m.fullscreenInspect {
		return m.renderFullscreenInspect()
	}

	// Fullscreen logs view
	if m.fullscreenLogs {
		return m.renderFullscreenLogs()
	}

	// Calculate panel widths
	leftWidth := m.width / 2
	rightWidth := m.width - leftWidth - 1 // -1 for separator
	if leftWidth < 20 {
		leftWidth = 20
	}
	if rightWidth < 20 {
		rightWidth = 20
	}

	// Calculate visible lines (2 header lines, 1 less for terminal)
	visibleLines := m.height - 3
	if visibleLines < 1 || m.height == 0 {
		visibleLines = 20
	}

	// Build left panel (tree view)
	var leftLines []string

	title := "Docker Swarm Services"
	if m.viewMode == ViewByNode {
		title = "Docker Swarm Nodes"
	}
	scrollInfo := ""
	if len(m.flatList) > 0 {
		scrollInfo = fmt.Sprintf(" [%d/%d]", m.cursor+1, len(m.flatList))
	}
	autoRefreshIndicator := ""
	if m.autoRefresh {
		autoRefreshIndicator = " [AUTO]"
	}
	leftLines = append(leftLines, dimStyle.Render(title+scrollInfo+autoRefreshIndicator))
	leftLines = append(leftLines, dimStyle.Render("j/k:nav gg/G:jump yy:copy enter:logs I:inspect n:mode a:auto W:wrap r:refresh q:quit"))

	start := m.offset
	if start < 0 {
		start = 0
	}

	// Render items until we fill the visible area
	// (items with errors take 2 lines, so we can't just use a fixed count)
	renderedLines := 0
	for i := start; i < len(m.flatList) && renderedLines < visibleLines; i++ {
		flat := m.flatList[i]
		if flat.node == nil {
			continue
		}
		line := m.renderNode(flat.node, flat.depth, flat.isLast)
		// Split on newlines (error messages add extra lines)
		parts := strings.Split(line, "\n")
		for j, part := range parts {
			if renderedLines >= visibleLines {
				break
			}
			if i == m.cursor && j == 0 {
				part = selectedStyle.Render(part)
			}
			leftLines = append(leftLines, part)
			renderedLines++
		}
	}

	// Build right panel (logs)
	var rightLines []string
	rightLines = append(rightLines, dimStyle.Render("Logs"))
	if m.logsLoading {
		rightLines = append(rightLines, dimStyle.Render("Loading..."))
	} else if len(m.logs) == 0 {
		rightLines = append(rightLines, dimStyle.Render("Select a task to view logs"))
	} else {
		logsScrollInfo := fmt.Sprintf(" [%d/%d]", m.logsOffset+1, len(m.logs))
		rightLines[0] = dimStyle.Render("Logs" + logsScrollInfo)
	}

	// Add log lines
	logsStart := m.logsOffset
	if logsStart < 0 {
		logsStart = 0
	}
	logsEnd := logsStart + visibleLines
	if logsEnd > len(m.logs) {
		logsEnd = len(m.logs)
	}
	for i := logsStart; i < logsEnd; i++ {
		line := sanitizeLine(m.logs[i])
		line = stripDockerTimestamp(line)
		if m.lineWrap {
			wrapped := wrapLine(line, rightWidth)
			rightLines = append(rightLines, wrapped...)
		} else {
			rightLines = append(rightLines, line)
		}
	}

	// Combine panels
	var output strings.Builder
	separator := "│"

	// Use height-1 to avoid last line being cut off by terminal
	numLines := m.height
	if numLines > 0 {
		numLines--
	}

	for i := 0; i < numLines; i++ {
		leftLine := ""
		if i < len(leftLines) {
			leftLine = leftLines[i]
		}
		rightLine := ""
		if i < len(rightLines) {
			rightLine = rightLines[i]
		}

		// Truncate/pad left panel to width
		leftLine = truncateOrPad(leftLine, leftWidth)
		// Truncate right panel
		rightLine = truncateLine(rightLine, rightWidth)

		output.WriteString(leftLine)
		output.WriteString(dimStyle.Render(separator))
		output.WriteString(rightLine)
		if i < numLines-1 {
			output.WriteString("\n")
		}
	}

	return output.String()
}

func (m Model) renderFullscreenLogs() string {
	visibleLines := m.height - 3
	if visibleLines < 1 {
		visibleLines = 1
	}

	var lines []string

	// Header with selected item name
	selectedName := ""
	if m.cursor < len(m.flatList) && m.flatList[m.cursor].node != nil {
		selectedName = m.flatList[m.cursor].node.Name
	}
	scrollInfo := fmt.Sprintf(" [%d/%d]", m.logsOffset+1, len(m.logs))
	autoIndicator := ""
	if m.autoRefresh {
		autoIndicator = " [AUTO]"
	}
	lines = append(lines, dimStyle.Render("Logs: "+selectedName+scrollInfo+autoIndicator))
	lines = append(lines, dimStyle.Render("j/k:scroll gg/G:jump yy:copy a:auto W:wrap q/esc:exit"))

	// Log content
	logsStart := m.logsOffset
	if logsStart < 0 {
		logsStart = 0
	}
	logsEnd := logsStart + visibleLines
	if logsEnd > len(m.logs) {
		logsEnd = len(m.logs)
	}
	for i := logsStart; i < logsEnd; i++ {
		line := sanitizeLine(m.logs[i])
		line = formatLogTimestamp(line)
		if m.lineWrap {
			wrapped := wrapLine(line, m.width-1)
			lines = append(lines, wrapped...)
		} else {
			line = truncateLine(line, m.width-1)
			lines = append(lines, line)
		}
	}

	// Build output
	var output strings.Builder
	numLines := m.height
	if numLines > 0 {
		numLines--
	}

	for i := 0; i < numLines; i++ {
		if i < len(lines) {
			output.WriteString(lines[i])
		}
		if i < numLines-1 {
			output.WriteString("\n")
		}
	}

	return output.String()
}

func (m Model) renderFullscreenInspect() string {
	visibleLines := m.height - 3
	if visibleLines < 1 {
		visibleLines = 1
	}

	var lines []string

	selectedName := ""
	if m.cursor < len(m.flatList) && m.flatList[m.cursor].node != nil {
		selectedName = m.flatList[m.cursor].node.Name
	}

	header := "Inspect: " + selectedName
	if m.inspectLoading {
		header += " (loading...)"
	} else if len(m.inspectLines) > 0 {
		header += fmt.Sprintf(" [%d/%d]", m.inspectOffset+1, len(m.inspectLines))
	}
	lines = append(lines, dimStyle.Render(header))
	lines = append(lines, dimStyle.Render("j/k:scroll gg/G:jump yy:copy W:wrap q/esc/I:exit"))

	if m.inspectLoading {
		lines = append(lines, dimStyle.Render("Loading container inspect..."))
	} else {
		start := m.inspectOffset
		if start < 0 {
			start = 0
		}
		end := start + visibleLines
		if end > len(m.inspectLines) {
			end = len(m.inspectLines)
		}
		for i := start; i < end; i++ {
			line := m.inspectLines[i]
			if m.lineWrap {
				wrapped := wrapLine(line, m.width-1)
				lines = append(lines, wrapped...)
			} else {
				line = truncateLine(line, m.width-1)
				lines = append(lines, line)
			}
		}
	}

	var output strings.Builder
	numLines := m.height
	if numLines > 0 {
		numLines--
	}

	for i := 0; i < numLines; i++ {
		if i < len(lines) {
			output.WriteString(lines[i])
		}
		if i < numLines-1 {
			output.WriteString("\n")
		}
	}

	return output.String()
}

func truncateOrPad(s string, width int) string {
	// Get visible length (without ANSI codes)
	visLen := visibleLength(s)
	if visLen >= width {
		return truncateLine(s, width)
	}
	// Reset formatting, then pad, to prevent color bleed
	return s + "\x1b[0m" + strings.Repeat(" ", width-visLen)
}

func wrapLine(s string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	if visibleLength(s) <= width {
		return []string{s}
	}
	var result []string
	for visibleLength(s) > width {
		// Find the rune index where we hit 'width' visible characters
		visCount := 0
		inEscape := false
		runes := []rune(s)
		cutIdx := len(runes)
		for i := 0; i < len(runes); i++ {
			r := runes[i]
			if r == '\x1b' {
				inEscape = true
				continue
			}
			if inEscape {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
					inEscape = false
				}
				continue
			}
			visCount++
			if visCount >= width {
				cutIdx = i + 1
				break
			}
		}
		result = append(result, string(runes[:cutIdx])+"\x1b[0m")
		s = "  " + string(runes[cutIdx:])
	}
	if len(s) > 0 {
		result = append(result, s)
	}
	return result
}

func truncateLine(s string, width int) string {
	if width <= 0 {
		return ""
	}

	// ANSI-aware truncation
	var result strings.Builder
	visCount := 0
	inEscape := false
	runes := []rune(s)

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if r == '\x1b' {
			inEscape = true
			result.WriteRune(r)
			continue
		}

		if inEscape {
			result.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}

		if visCount >= width-1 {
			result.WriteString("…")
			break
		}

		result.WriteRune(r)
		visCount++
	}

	// Always reset formatting at end to prevent bleed
	result.WriteString("\x1b[0m")
	return result.String()
}

func visibleLength(s string) int {
	// Strip ANSI codes and count
	inEscape := false
	count := 0
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		count++
	}
	return count
}

func sanitizeLine(s string) string {
	// Remove carriage returns and other control characters that break layout
	var result strings.Builder
	for _, r := range s {
		switch {
		case r == '\r':
			// Skip carriage returns
			continue
		case r == '\t':
			// Replace tabs with spaces
			result.WriteString("    ")
		case r < 32 && r != '\x1b':
			// Skip other control characters except ESC (for ANSI codes)
			continue
		default:
			result.WriteRune(r)
		}
	}
	return result.String()
}

// stripDockerTimestamp removes the Docker timestamp prefix from a log line
func stripDockerTimestamp(line string) string {
	if len(line) < 30 {
		return line
	}

	// Find the space after the timestamp
	spaceIdx := strings.Index(line, " ")
	if spaceIdx < 20 || spaceIdx > 35 {
		return line
	}

	// Verify it looks like a timestamp
	_, err := time.Parse(time.RFC3339Nano, line[:spaceIdx])
	if err != nil {
		return line
	}

	return line[spaceIdx+1:]
}

// formatLogTimestamp reformats Docker log timestamps (RFC3339Nano) to HH:MM:SS.mmm format
func formatLogTimestamp(line string) string {
	// Docker timestamps are at the start: 2024-01-28T15:04:05.123456789Z message
	if len(line) < 30 {
		return line
	}

	// Find the space after the timestamp
	spaceIdx := strings.Index(line, " ")
	if spaceIdx < 20 || spaceIdx > 35 {
		return line
	}

	timestampStr := line[:spaceIdx]
	rest := line[spaceIdx+1:]

	// Parse the timestamp
	t, err := time.Parse(time.RFC3339Nano, timestampStr)
	if err != nil {
		return line
	}

	// Format as HH:MM:SS.mmm
	formatted := t.Format("15:04:05.000")
	return dimStyle.Render(formatted) + " " + rest
}

func (m Model) renderNode(node *TreeNode, depth int, isLast bool) string {
	indent := strings.Repeat("  ", depth)

	if node.IsParent {
		prefix := "▼"
		if len(node.Children) == 0 {
			prefix = "○"
		}

		var name string
		var info string
		if m.viewMode == ViewByNode {
			name = nodeStyle.Render(node.Name)
			stateColor := taskRunningStyle
			if node.State != "ready" {
				stateColor = taskFailedStyle
			}
			// Apply dimStyle to brackets separately to avoid color reset issues
			info = dimStyle.Render(" [") + stateColor.Render(node.State) + dimStyle.Render(fmt.Sprintf("] (%s) %s", node.Replicas, node.Image))
		} else {
			name = serviceStyle.Render(node.Name)
			info = dimStyle.Render(fmt.Sprintf(" (%s) %s", node.Replicas, node.Image))
		}
		return fmt.Sprintf("%s%s %s%s", indent, prefix, name, info)
	}

	// Task node
	prefix := "├─"
	if isLast {
		prefix = "└─"
	}

	var stateStyle lipgloss.Style
	switch node.State {
	case "running":
		stateStyle = taskRunningStyle
	case "ready":
		stateStyle = taskReadyStyle
	case "starting", "preparing", "assigned", "accepted", "pending":
		stateStyle = taskStartingStyle
	case "failed", "rejected":
		stateStyle = taskFailedStyle
	default:
		stateStyle = taskOtherStyle
	}

	state := stateStyle.Render(node.State)

	var extra string
	if m.viewMode == ViewByNode {
		// In node view, show service image instead of node name
		extra = dimStyle.Render(fmt.Sprintf(" %s", truncateImage(node.Image)))
	} else {
		// In service view, show node name
		if node.NodeName != "" {
			extra = dimStyle.Render(fmt.Sprintf(" @%s", node.NodeName))
		}
	}

	line := fmt.Sprintf("%s%s %s [%s]%s", indent, prefix, node.Name, state, extra)

	if node.Error != "" {
		continueChar := "│"
		if isLast {
			continueChar = " "
		}
		errLine := fmt.Sprintf("\n%s%s  %s", indent, continueChar, errorStyle.Render("↳ "+node.Error))
		line += errLine
	}

	return line
}
