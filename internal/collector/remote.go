package collector

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/wjzhangq/gpumon/internal/config"
	"github.com/wjzhangq/gpumon/internal/model"
)

// Remote 通过 SSH 采集远程 Linux 机器的指标，**不在对端安装任何 agent**。
//
// 设计要点：
//   - 复用一条长连接，每个采集周期只开一个 session；连接断了下个周期自动重连。
//   - 静态信息（主机名、CPU 拓扑、机型）只在建连时抓一次并缓存，
//     每周期的脚本只读 /proc/stat、/proc/meminfo、df 和 nvidia-smi。
//   - CPU 使用率用两次 /proc/stat 的差值算，跨采集周期做差，
//     所以脚本里不需要 sleep，采集延迟只有一个 RTT。
type Remote struct {
	name       string
	addr       string
	user       string
	clientCfg  *ssh.ClientConfig
	diskFilter map[string]bool
	diskMode   string
	dynScript  string

	mu       sync.Mutex
	client   *ssh.Client
	static   *remoteStatic
	prevStat map[string]cpuTimes
}

type remoteStatic struct {
	host model.HostInfo
	topo []socketInfo
}

// NewRemote 构造远程采集器。这里只做参数解析（读私钥、建 host key 回调），
// 不会发起网络连接——首次连接推迟到第一次 Collect。
func NewRemote(n config.Node) (*Remote, error) {
	s := n.SSH
	if s == nil {
		return nil, fmt.Errorf("节点 %s: 缺少 ssh 配置", n.Name)
	}

	auths, err := buildAuthMethods(s)
	if err != nil {
		return nil, fmt.Errorf("节点 %s: %w", n.Name, err)
	}
	hostKeyCB, err := buildHostKeyCallback(s)
	if err != nil {
		return nil, fmt.Errorf("节点 %s: %w", n.Name, err)
	}

	nvsmi := n.NvidiaSmi
	if nvsmi == "" {
		nvsmi = "nvidia-smi"
	}

	return &Remote{
		name:       n.Name,
		addr:       s.Addr(),
		user:       s.User,
		clientCfg: &ssh.ClientConfig{
			User:            s.User,
			Auth:            auths,
			HostKeyCallback: hostKeyCB,
			Timeout:         s.ConnectTimeout.Or(config.DefaultConnectTimeout),
		},
		diskFilter: toSet(n.Disks),
		diskMode:   n.DiskMode,
		dynScript:  buildDynamicScript(nvsmi, n.DiskMode),
		prevStat:   make(map[string]cpuTimes),
	}, nil
}

// Name 实现 Collector。
func (r *Remote) Name() string { return r.name }

// Close 实现 Collector，关闭底层 SSH 连接。
func (r *Remote) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.disconnectLocked()
}

// Collect 实现 Collector。
func (r *Remote) Collect(ctx context.Context) (model.Snapshot, error) {
	start := time.Now()
	snap := model.Snapshot{Node: r.name, Timestamp: start}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.connectLocked(ctx); err != nil {
		return r.fail(snap, start, err)
	}

	out, err := runScript(ctx, r.client, r.dynScript)
	if err != nil {
		// 连接可能已经坏了，丢掉重来。
		_ = r.disconnectLocked()
		return r.fail(snap, start, err)
	}

	sections := splitSections(out)
	if _, ok := sections["END"]; !ok {
		_ = r.disconnectLocked()
		return r.fail(snap, start, fmt.Errorf("远程脚本输出不完整（未见结束标记），对端可能被 kill 或 shell 异常"))
	}

	snap.Online = true
	snap.Host = r.static.host
	snap.CPUs = r.cpusFromStat(sections["STAT"])
	snap.Memory = parseMeminfo(sections["MEM"])
	if r.diskMode == config.DiskModeBlock {
		snap.Disks = parseBlockDevices(sections["DISK"], r.diskFilter)
	} else {
		snap.Disks = parseDF(sections["DISK"], r.diskFilter)
	}
	snap.GPUs = parseNvidiaCSV(strings.Join(sections["GPU"], "\n"))

	if model.DetectUnifiedMemory(snap.Host.Arch, snap.Memory.TotalBytes, snap.GPUs) {
		snap.Memory.Unified = true
		for i := range snap.GPUs {
			snap.GPUs[i].Unified = true
		}
	}

	snap.Timestamp = time.Now()
	snap.CollectMS = time.Since(start).Milliseconds()
	return snap, nil
}

func (r *Remote) fail(snap model.Snapshot, start time.Time, err error) (model.Snapshot, error) {
	snap.Online = false
	snap.Error = err.Error()
	snap.Timestamp = time.Now()
	snap.CollectMS = time.Since(start).Milliseconds()
	// 保留已知的静态信息，方便前端即使离线也能显示机器名。
	if r.static != nil {
		snap.Host = r.static.host
	}
	return snap, err
}

// connectLocked 确保连接可用并已缓存静态信息。调用方需持有 r.mu。
func (r *Remote) connectLocked(ctx context.Context) error {
	if r.client != nil && r.static != nil {
		return nil
	}
	if r.client == nil {
		var d net.Dialer
		d.Timeout = r.clientCfg.Timeout

		conn, err := d.DialContext(ctx, "tcp", r.addr)
		if err != nil {
			return fmt.Errorf("连接 %s 失败: %w", r.addr, err)
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}

		c, chans, reqs, err := ssh.NewClientConn(conn, r.addr, r.clientCfg)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("SSH 握手失败 %s@%s: %w", r.user, r.addr, err)
		}
		// 握手完成后清掉临时 deadline，否则长连接会被读超时打断。
		_ = conn.SetDeadline(time.Time{})

		r.client = ssh.NewClient(c, chans, reqs)
		// 新连接意味着可能是对端重启，CPU 累计值不能再跨连接做差。
		r.prevStat = make(map[string]cpuTimes)
		r.static = nil
	}

	if r.static == nil {
		st, err := r.fetchStaticLocked(ctx)
		if err != nil {
			_ = r.disconnectLocked()
			return err
		}
		r.static = st
	}
	return nil
}

func (r *Remote) fetchStaticLocked(ctx context.Context) (*remoteStatic, error) {
	out, err := runScript(ctx, r.client, staticScript)
	if err != nil {
		return nil, fmt.Errorf("采集静态信息失败: %w", err)
	}
	sections := splitSections(out)

	hostKV := parseKeyValues(sections["HOST"])
	h := model.HostInfo{
		Hostname: hostKV["hostname"],
		OS:       strings.ToLower(firstNonEmpty(hostKV["os"], "linux")),
		Platform: hostKV["platform"],
		Kernel:   hostKV["kernel"],
		Arch:     hostKV["arch"],
		Model:    hostKV["model"],
	}
	if h.Hostname == "" {
		h.Hostname = r.addr
	}

	entries := parseProcCPUInfo(sections["CPUINFO"])
	lscpu := parseLscpu(sections["LSCPU"])
	topo := buildRemoteTopology(entries, lscpu, h)

	return &remoteStatic{host: h, topo: topo}, nil
}

func (r *Remote) disconnectLocked() error {
	r.static = nil
	if r.client == nil {
		return nil
	}
	err := r.client.Close()
	r.client = nil
	return err
}

// cpusFromStat 把 /proc/stat 的累计值转换成本周期的每 socket 使用率。
func (r *Remote) cpusFromStat(lines []string) []model.CPU {
	cur := parseProcStat(lines)

	out := make([]model.CPU, 0, len(r.static.topo))
	for i, s := range r.static.topo {
		c := model.CPU{
			Index:         i,
			Model:         s.model,
			PhysicalCores: s.physCores,
			LogicalCores:  len(s.logical),
		}
		var sum float64
		var n int
		for _, idx := range s.logical {
			key := fmt.Sprintf("cpu%d", idx)
			u, ok := usageBetween(r.prevStat[key], cur[key])
			if ok {
				sum += u
				n++
			}
		}
		if n > 0 {
			c.UsagePercent = round2(clampPercent(sum / float64(n)))
		} else if u, ok := usageBetween(r.prevStat["cpu"], cur["cpu"]); ok {
			// 逐核数据对不上时（拓扑解析失败等），退回整机平均值。
			c.UsagePercent = round2(u)
		}
		out = append(out, c)
	}

	r.prevStat = cur
	return out
}

// ---------------------------------------------------------------------------
// SSH 辅助
// ---------------------------------------------------------------------------

func buildAuthMethods(s *config.SSH) ([]ssh.AuthMethod, error) {
	var auths []ssh.AuthMethod

	if s.KeyFile != "" {
		key, err := os.ReadFile(s.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("读取私钥 %s 失败: %w", s.KeyFile, err)
		}
		var signer ssh.Signer
		if s.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(s.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			return nil, fmt.Errorf("解析私钥 %s 失败（带口令的密钥需要配置 ssh.passphrase）: %w", s.KeyFile, err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}

	if s.UseAgent {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, fmt.Errorf("配置了 ssh.use_agent 但环境变量 SSH_AUTH_SOCK 为空")
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, fmt.Errorf("连接 ssh-agent (%s) 失败: %w", sock, err)
		}
		auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
	}

	if s.Password != "" {
		auths = append(auths, ssh.Password(s.Password))
	}

	if len(auths) == 0 {
		return nil, fmt.Errorf("没有可用的 SSH 认证方式")
	}
	return auths, nil
}

func buildHostKeyCallback(s *config.SSH) (ssh.HostKeyCallback, error) {
	if s.InsecureSkipHostKeyCheck {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	cb, err := knownhosts.New(s.KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("加载 known_hosts (%s) 失败: %w（先手动 ssh 一次录入指纹，或设置 insecure_skip_host_key_check: true）", s.KnownHostsFile, err)
	}
	return cb, nil
}

// runScript 在一个新 session 上执行脚本，并支持 context 取消。
func runScript(ctx context.Context, client *ssh.Client, script string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("SSH 连接未建立")
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("创建 SSH session 失败: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(script) }()

	select {
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg != "" {
				return stdout.String(), fmt.Errorf("远程命令执行失败: %v (%s)", err, truncate(msg, 300))
			}
			return stdout.String(), fmt.Errorf("远程命令执行失败: %w", err)
		}
		return stdout.String(), nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return "", fmt.Errorf("远程采集超时: %w", ctx.Err())
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
