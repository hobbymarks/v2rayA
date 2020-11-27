package v2ray

import (
	"fmt"
	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/common/netTools/netstat"
	"github.com/v2rayA/v2rayA/common/netTools/ports"
	"github.com/v2rayA/v2rayA/core/dnsPoison/entity"
	"github.com/v2rayA/v2rayA/core/iptables"
	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/extra/copyfile"
	"github.com/v2rayA/v2rayA/global"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"
)

const (
	originResolverFile = "/etc/resolv.conf"
	bakResolverFile    = "/etc/resolv.conf.v2rayabak"
)

func HijackDNS() error {
	RestoreDNS()
	err := copyfile.CopyFile(originResolverFile, bakResolverFile)
	if err != nil {
		return fmt.Errorf("failed to hijackDNS: [copy] %v", err)
	}
	err = os.Remove(originResolverFile)
	if err != nil {
		return fmt.Errorf("failed to hijackDNS: [remove]%v", err)
	}
	err = ioutil.WriteFile(originResolverFile, []byte("nameserver 2.0.1.7\n"), os.FileMode(0644))
	//err = ioutil.WriteFile(originResolverFile, []byte("nameserver 208.67.222.220\n"), os.FileMode(0644))
	if err != nil {
		_ = os.Remove(bakResolverFile)
		err = fmt.Errorf("failed to hijackDNS: [write] %v", err)
	}
	return err
}

func RestoreDNS() {
	err := copyfile.CopyFile(bakResolverFile, originResolverFile)
	if err != nil {
		return
	}
	_ = os.Remove(bakResolverFile)
}

func DeleteTransparentProxyRules() {
	iptables.Tproxy.GetCleanCommands().Clean()
	iptables.Redirect.GetCleanCommands().Clean()
	iptables.DropSpoofing.GetCleanCommands().Clean()
	RestoreDNS()
	time.Sleep(100 * time.Millisecond)
}

func WriteTransparentProxyRules(preprocess *func(c *iptables.SetupCommands)) error {
	if entity.ShouldDnsPoisonOpen() {
		if e := iptables.DropSpoofing.GetSetupCommands().Setup(preprocess); e != nil {
			log.Println(newError("[WARNING] DropSpoofing can't be enable").Base(e))
			iptables.DropSpoofing.GetCleanCommands().Clean()
		}
	}
	setting := configure.GetSettingNotNil()
	if global.SupportTproxy && !setting.EnhancedMode {
		if err := iptables.Tproxy.GetSetupCommands().Setup(preprocess); err == nil {
			if setting.AntiPollution != configure.AntipollutionClosed {
				err = HijackDNS()
				if err != nil {
					return fmt.Errorf("cannot hijack system dns: %v", err)
				}
			}
		} else {
			if strings.Contains(err.Error(), "TPROXY") && strings.Contains(err.Error(), "No chain") {
				err = newError("not compile xt_TPROXY in kernel")
			}
			DeleteTransparentProxyRules()
			log.Println(err)
			global.SupportTproxy = false
		}
	} else {
		if err := iptables.Redirect.GetSetupCommands().Setup(preprocess); err != nil {
			log.Println(err)
			DeleteTransparentProxyRules()
			return newError("not support transparent proxy: ").Base(err)
		}
	}
	return nil
}

func nextPortsGroup(ports []string, groupSize int) (group []string, remain []string) {
	var cnt int
	for i := range ports {
		if strings.ContainsRune(ports[i], ':') {
			cnt += 2
		} else {
			cnt++
		}
		if cnt == groupSize {
			return ports[:i+1], ports[i+1:]
		} else if cnt > groupSize {
			return ports[:i], ports[i:]
		}
	}
	if len(ports) > 0 {
		return ports, nil
	}
	return nil, nil
}

func CheckAndSetupTransparentProxy(checkRunning bool) (err error) {
	setting := configure.GetSettingNotNil()
	preprocess := func(c *iptables.SetupCommands) {
		commands := string(*c)
		//先看要不要把自己的端口加进去
		selfPort := strings.Split(global.GetEnvironmentConfig().Address, ":")[1]
		wl := configure.GetPortWhiteListNotNil()
		if !wl.Has(selfPort, "tcp") {
			wl.TCP = append(wl.TCP, selfPort)
		}
		lines := strings.Split(commands, "\n")
		for i, line := range lines {
			if strings.Contains(line, "{{TCP_PORTS}}") {
				raw := line
				lines[i] = ""
				var grp []string
				r := wl.TCP
				for r != nil {
					grp, r = nextPortsGroup(r, 15)
					if grp != nil {
						lines[i] += strings.Replace(raw, "{{TCP_PORTS}}", strings.Join(grp, ","), 1) + "\n"
					}
				}
				lines[i] = strings.TrimSuffix(lines[i], "\n")
			} else if strings.Contains(line, "{{UDP_PORTS}}") {
				raw := line
				lines[i] = ""
				var grp []string
				r := wl.UDP
				for r != nil {
					grp, r = nextPortsGroup(r, 15)
					if grp != nil {
						lines[i] += strings.Replace(raw, "{{UDP_PORTS}}", strings.Join(grp, ","), 1) + "\n"
					}
				}
				lines[i] = strings.TrimSuffix(lines[i], "\n")
			}
		}
		commands = strings.Join(lines, "\n")
		if setting.AntiPollution == configure.AntipollutionClosed {
			commands = common.TrimLineContains(commands, "udp")
		}
		if entity.ShouldDnsPoisonOpen() {
			commands = common.TrimLineContains(commands, "240.0.0.0/4")
		}
		*c = iptables.SetupCommands(commands)
	}
	if (!checkRunning || IsV2RayRunning()) && setting.Transparent != configure.TransparentClose {
		var (
			o bool
			s *netstat.Socket
		)
		o, s, err = ports.IsPortOccupied([]string{"32345:tcp,udp"})
		if err != nil {
			return
		}
		if o {
			p, e := s.Process()
			if e == nil && p.Name != "v2ray" {
				err = newError("transparent proxy cannot be set up, port 32345 is occupied by ", p.Name)
				return
			}
		}
		DeleteTransparentProxyRules()
		err = WriteTransparentProxyRules(&preprocess)
	}
	return
}

func CheckAndStopTransparentProxy() {
	DeleteTransparentProxyRules()
}
