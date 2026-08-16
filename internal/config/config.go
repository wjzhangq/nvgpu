// Package config 负责加载与校验 YAML 配置。
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 默认值与硬上限。
const (
	// MaxHistorySize 是需求规定的历史点数上限：最多 60 个采集周期。
	MaxHistorySize = 60

	DefaultListen         = "0.0.0.0:8080"
	DefaultInterval       = 5 * time.Second
	DefaultCollectTimeout = 10 * time.Second
	DefaultConnectTimeout = 8 * time.Second
	DefaultSSHPort        = 22

	MinInterval = 500 * time.Millisecond
)

// 节点类型。
const (
	TypeLocal = "local"
	TypeSSH   = "ssh"
)

// 磁盘采集模式。
const (
	// DiskModeMount 按挂载点采集文件系统容量（默认，历史行为）。
	DiskModeMount = "mount"
	// DiskModeBlock 按物理块设备采集磁盘原始容量，不关心挂载目录。
	DiskModeBlock = "block"
)

// Duration 让 YAML 里可以直接写 "5s" / "1m30s"，也兼容纯数字（按秒解析）。
type Duration struct {
	time.Duration
}

// UnmarshalYAML 实现 yaml.Unmarshaler。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			d.Duration = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("非法的时间间隔 %q: %w", s, err)
		}
		d.Duration = v
		return nil
	}

	var secs float64
	if err := node.Decode(&secs); err != nil {
		return fmt.Errorf("非法的时间间隔 %q（请使用 \"5s\" 这样的格式）", node.Value)
	}
	d.Duration = time.Duration(secs * float64(time.Second))
	return nil
}

// MarshalYAML 实现 yaml.Marshaler。
func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

// Or 返回自身，若未设置（<=0）则返回 def。
func (d Duration) Or(def time.Duration) time.Duration {
	if d.Duration <= 0 {
		return def
	}
	return d.Duration
}

// Config 是配置文件根结构。
type Config struct {
	Server   Server   `yaml:"server"`
	Defaults Defaults `yaml:"defaults"`
	Nodes    []Node   `yaml:"nodes"`
}

// Server 是 HTTP 服务配置。
type Server struct {
	Listen string `yaml:"listen"`
	// CORS 为 nil 时默认开启（与 gpu2 的 server.py 行为一致）。
	CORS *bool `yaml:"cors"`
}

// CORSEnabled 返回是否开启跨域响应头。
func (s Server) CORSEnabled() bool {
	if s.CORS == nil {
		return true
	}
	return *s.CORS
}

// Defaults 是所有节点共享的默认值，可被单个节点覆盖。
type Defaults struct {
	Interval       Duration `yaml:"interval"`
	HistorySize    int      `yaml:"history_size"`
	CollectTimeout Duration `yaml:"collect_timeout"`
}

// Node 描述一台被纳管的机器。
type Node struct {
	// Name 是这台机器在 API 里的唯一标识，由配置文件指定（不再用随机 UUID）。
	Name string `yaml:"name"`
	// Type 取值 local 或 ssh；留空时按是否配置了 ssh 段自动推断。
	Type string `yaml:"type"`
	// Interval 覆盖 defaults.interval。
	Interval Duration `yaml:"interval"`
	// DiskMode 磁盘采集模式："mount"（挂载点，默认）或 "block"（物理磁盘）。
	DiskMode string `yaml:"disk_mode"`
	// Disks 白名单：mount 模式下是挂载点（如 ["/", "/data"]），
	// block 模式下是块设备名（如 ["nvme0n1", "sda"]，支持带 /dev/ 前缀）。
	// 留空表示自动发现。
	Disks []string `yaml:"disks"`
	// NvidiaSmi 指定 nvidia-smi 路径；留空时自动探测（远程默认用 PATH）。
	NvidiaSmi string `yaml:"nvidia_smi"`
	// PartitionCacheTTL 挂载点缓存 TTL（仅 type=local 且 disk_mode=mount 时有效）。
	// 留空使用默认 60s，显式设为 0 禁用缓存。挂载点变化频率极低，缓存可节省 5-20ms。
	PartitionCacheTTL *Duration `yaml:"partition_cache_ttl"`
	// SSH 仅在 type=ssh 时有效。
	SSH *SSH `yaml:"ssh"`
}

// SSH 是远程节点的连接参数。
type SSH struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	User       string `yaml:"user"`
	Password   string `yaml:"password"`
	KeyFile    string `yaml:"key_file"`
	Passphrase string `yaml:"passphrase"`
	UseAgent   bool   `yaml:"use_agent"`

	KnownHostsFile           string `yaml:"known_hosts_file"`
	InsecureSkipHostKeyCheck bool   `yaml:"insecure_skip_host_key_check"`

	ConnectTimeout Duration `yaml:"connect_timeout"`
}

// Addr 返回 host:port。
func (s SSH) Addr() string {
	port := s.Port
	if port == 0 {
		port = DefaultSSHPort
	}
	return fmt.Sprintf("%s:%d", s.Host, port)
}

// Load 读取并校验配置文件。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// 严格模式：拼错的字段直接报错，而不是被静默忽略。
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) normalize() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Defaults.Interval.Duration <= 0 {
		c.Defaults.Interval = Duration{DefaultInterval}
	}
	if c.Defaults.CollectTimeout.Duration <= 0 {
		c.Defaults.CollectTimeout = Duration{DefaultCollectTimeout}
	}

	switch {
	case c.Defaults.HistorySize <= 0:
		c.Defaults.HistorySize = MaxHistorySize
	case c.Defaults.HistorySize > MaxHistorySize:
		c.Defaults.HistorySize = MaxHistorySize
	}

	if len(c.Nodes) == 0 {
		return fmt.Errorf("配置文件中至少需要一个 node")
	}

	seen := make(map[string]bool, len(c.Nodes))
	localCount := 0

	for i := range c.Nodes {
		n := &c.Nodes[i]

		n.Name = strings.TrimSpace(n.Name)
		if n.Name == "" {
			return fmt.Errorf("nodes[%d]: name 不能为空", i)
		}
		if seen[n.Name] {
			return fmt.Errorf("nodes[%d]: 节点名 %q 重复", i, n.Name)
		}
		seen[n.Name] = true

		n.Type = strings.ToLower(strings.TrimSpace(n.Type))
		if n.Type == "" {
			if n.SSH != nil {
				n.Type = TypeSSH
			} else {
				n.Type = TypeLocal
			}
		}

		// 规范化 disk_mode
		n.DiskMode = strings.ToLower(strings.TrimSpace(n.DiskMode))
		if n.DiskMode == "" {
			n.DiskMode = DiskModeMount // 默认挂载点模式，向后兼容
		}
		if n.DiskMode != DiskModeMount && n.DiskMode != DiskModeBlock {
			return fmt.Errorf("nodes[%d] (%s): disk_mode 只能是 %q 或 %q，得到 %q",
				i, n.Name, DiskModeMount, DiskModeBlock, n.DiskMode)
		}

		switch n.Type {
		case TypeLocal:
			localCount++
			if localCount > 1 {
				return fmt.Errorf("nodes[%d] (%s): 只允许配置一个 type=local 的节点", i, n.Name)
			}
			if n.SSH != nil {
				return fmt.Errorf("nodes[%d] (%s): type=local 不能同时配置 ssh 段", i, n.Name)
			}
			n.NvidiaSmi = expandPath(n.NvidiaSmi)

		case TypeSSH:
			if n.SSH == nil {
				return fmt.Errorf("nodes[%d] (%s): type=ssh 必须配置 ssh 段", i, n.Name)
			}
			s := n.SSH
			if strings.TrimSpace(s.Host) == "" {
				return fmt.Errorf("nodes[%d] (%s): ssh.host 不能为空", i, n.Name)
			}
			if strings.TrimSpace(s.User) == "" {
				return fmt.Errorf("nodes[%d] (%s): ssh.user 不能为空", i, n.Name)
			}
			if s.Port == 0 {
				s.Port = DefaultSSHPort
			}
			if s.ConnectTimeout.Duration <= 0 {
				s.ConnectTimeout = Duration{DefaultConnectTimeout}
			}
			if s.KeyFile == "" && s.Password == "" && !s.UseAgent {
				return fmt.Errorf("nodes[%d] (%s): 必须至少提供 ssh.key_file / ssh.password / ssh.use_agent 之一", i, n.Name)
			}
			s.KeyFile = expandPath(s.KeyFile)
			s.KnownHostsFile = expandPath(s.KnownHostsFile)
			if !s.InsecureSkipHostKeyCheck && s.KnownHostsFile == "" {
				if home, err := os.UserHomeDir(); err == nil {
					s.KnownHostsFile = filepath.Join(home, ".ssh", "known_hosts")
				} else {
					return fmt.Errorf("nodes[%d] (%s): 无法定位默认 known_hosts，请显式配置 ssh.known_hosts_file 或设置 insecure_skip_host_key_check: true", i, n.Name)
				}
			}

		default:
			return fmt.Errorf("nodes[%d] (%s): 未知的 type %q（只支持 local / ssh）", i, n.Name, n.Type)
		}

		if n.Interval.Duration <= 0 {
			n.Interval = c.Defaults.Interval
		}
		if n.Interval.Duration < MinInterval {
			return fmt.Errorf("nodes[%d] (%s): interval 不能小于 %s", i, n.Name, MinInterval)
		}
	}

	return nil
}

// CollectTimeout 返回某节点的有效采集超时：不超过 defaults.collect_timeout，
// 也不超过该节点自身的采集间隔（避免任务堆积）。
func (c *Config) CollectTimeout(n Node) time.Duration {
	t := c.Defaults.CollectTimeout.Or(DefaultCollectTimeout)
	if n.Interval.Duration > 0 && n.Interval.Duration < t {
		return n.Interval.Duration
	}
	return t
}

// NodeNames 按配置顺序返回所有节点名。
func (c *Config) NodeNames() []string {
	out := make([]string, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		out = append(out, n.Name)
	}
	return out
}

func expandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimLeft(p[1:], `/\`))
		}
	}
	return p
}
