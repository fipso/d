package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
}

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

type logsLoadedMsg struct {
	taskID string
	lines  []string
	err    error
}

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
	// Parse subcommand
	args := os.Args[1:]
	viewMode := ViewByService

	// Check for help flag
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			printHelp()
			return
		}
	}

	// Check for subcommand
	if len(args) > 0 {
		switch args[0] {
		case "nodes":
			viewMode = ViewByNode
		default:
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", args[0])
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
    d [COMMAND] [OPTIONS]

COMMANDS:
    (none)        Show services with their tasks (default)
    nodes         Show nodes with their tasks

OPTIONS:
    -h, --help    Show this help message

KEYBINDINGS:
    j, ↓          Move cursor down
    k, ↑          Move cursor up
    gg            Jump to top
    G             Jump to bottom
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
		loading:  true,
		viewMode: viewMode,
	}
}

func (m Model) Init() tea.Cmd {
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

// getDockerHost resolves the Docker host from context configuration
func getDockerHost() (string, error) {
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

		serviceNode := &TreeNode{
			Name:      svc.Spec.Name,
			IsParent:  true,
			ServiceID: svc.ID,
			Replicas:  replicas,
			Image:     truncateImage(svc.Spec.TaskTemplate.ContainerSpec.Image),
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

		// Sort tasks by service name, slot, then newest first
		nodeTasks := tasksByNode[n.ID]
		sort.Slice(nodeTasks, func(i, j int) bool {
			svcI := serviceMap[nodeTasks[i].ServiceID]
			svcJ := serviceMap[nodeTasks[j].ServiceID]
			if svcI.Spec.Name != svcJ.Spec.Name {
				return svcI.Spec.Name < svcJ.Spec.Name
			}
			if nodeTasks[i].Slot != nodeTasks[j].Slot {
				return nodeTasks[i].Slot < nodeTasks[j].Slot
			}
			return nodeTasks[i].CreatedAt.After(nodeTasks[j].CreatedAt)
		})

		// Filter: show only running task OR most recent task per service+slot
		type slotKey struct {
			serviceID string
			slot      int
		}
		seenSlots := make(map[slotKey]bool)

		for _, task := range nodeTasks {
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
				Image:       truncateImage(svc.Spec.TaskTemplate.ContainerSpec.Image),
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

	case logsLoadedMsg:
		// Only update if this is for the currently selected task
		if msg.taskID == m.lastSelectedID {
			m.logsLoading = false
			if msg.err != nil {
				m.logs = []string{fmt.Sprintf("Error: %v", msg.err)}
			} else {
				m.logs = msg.lines
			}
			// Start at the bottom of logs (most recent)
			m.logsOffset = len(m.logs) - (m.height - 3)
			if m.logsOffset < 0 {
				m.logsOffset = 0
			}
		}
		return m, nil

	case tea.KeyMsg:
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
		}
	}

	return m, nil
}

func (m *Model) maybeLoadLogs() tea.Cmd {
	if m.cursor >= len(m.flatList) {
		return nil
	}
	node := m.flatList[m.cursor].node

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
	// ID used to match response with current selection
	nodeID := node.Name
	if !node.IsParent && node.TaskID != "" {
		nodeID = node.TaskID
	}

	isTask := !node.IsParent && node.TaskID != ""
	taskID := node.TaskID
	serviceName := node.Name // for parents, Name is the service name

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
			Tail:       "500",
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
			return logsLoadedMsg{taskID: nodeID, err: err}
		}
		defer reader.Close()

		// Read all data first
		data, err := io.ReadAll(reader)
		if err != nil {
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

		return logsLoadedMsg{taskID: nodeID, lines: lines}
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
	leftLines = append(leftLines, dimStyle.Render(title+scrollInfo))
	leftLines = append(leftLines, dimStyle.Render("j/k:nav gg/G:jump r:refresh q:quit"))

	start := m.offset
	if start < 0 {
		start = 0
	}

	// Render items until we fill the visible area
	// (items with errors take 2 lines, so we can't just use a fixed count)
	renderedLines := 0
	for i := start; i < len(m.flatList) && renderedLines < visibleLines; i++ {
		flat := m.flatList[i]
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
		rightLines = append(rightLines, line)
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

func truncateOrPad(s string, width int) string {
	// Get visible length (without ANSI codes)
	visLen := visibleLength(s)
	if visLen >= width {
		return truncateLine(s, width)
	}
	// Reset formatting, then pad, to prevent color bleed
	return s + "\x1b[0m" + strings.Repeat(" ", width-visLen)
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
