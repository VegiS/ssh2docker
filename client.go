package ssh2docker

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apex/log"
	"github.com/kr/pty"
	"github.com/moul/ssh2docker/pkg/envhelper"
	"github.com/moul/ssh2docker/pkg/ttyhelper"
	"golang.org/x/crypto/ssh"
)

var clientCounter = 0

// Client is one client connection
type Client struct {
	Idx        int
	ChannelIdx int
	Conn       *ssh.ServerConn
	Chans      <-chan ssh.NewChannel
	Reqs       <-chan *ssh.Request
	Server     *Server
	Pty, Tty   *os.File
	Config     *ClientConfig
	ClientID   string
}

type ClientConfig struct {
	ImageName              string                `json:"image-name,omitempty"`
	RemoteUser             string                `json:"remote-user,omitempty"`
	Env                    envhelper.Environment `json:"env,omitempty"`
	Command                []string              `json:"command,omitempty"`
	DockerRunArgs          []string              `json:"docker-run-args,omitempty"`
	User                   string                `json:"user,omitempty"`
	Keys                   []string              `json:"keys,omitempty"`
	AuthenticationMethod   string                `json:"authentication-method,omitempty"`
	AuthenticationComment  string                `json:"authentication-coment,omitempty"`
	EntryPoint             string                `json:"entrypoint,omitempty"`
	AuthenticationAttempts int                   `json:"authentication-attempts,omitempty"`
	Allowed                bool                  `json:"allowed,omitempty"`
	IsLocal                bool                  `json:"is_local,omitempty"`
}

// NewClient initializes a new client
func NewClient(conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, server *Server) *Client {
	client := Client{
		Idx:        clientCounter,
		ClientID:   conn.RemoteAddr().String(),
		ChannelIdx: 0,
		Conn:       conn,
		Chans:      chans,
		Reqs:       reqs,
		Server:     server,

		// Default ClientConfig, will be overwritten if a hook is used
		Config: &ClientConfig{
			ImageName:              strings.Replace(conn.User(), "_", "/", -1),
			RemoteUser:             "anonymous",
			AuthenticationMethod:   "noauth",
			AuthenticationComment:  "",
			AuthenticationAttempts: 0,
			Env:     envhelper.Environment{},
			Command: make([]string, 0),
		},
	}

	if server.LocalUser != "" {
		client.Config.IsLocal = client.Config.ImageName == server.LocalUser
	}

	if _, found := server.ClientConfigs[client.ClientID]; !found {
		server.ClientConfigs[client.ClientID] = client.Config
	}

	client.Config = server.ClientConfigs[conn.RemoteAddr().String()]
	client.Config.Env.ApplyDefaults()

	clientCounter++

	remoteAddr := strings.Split(client.ClientID, ":")
	log.Infof("Accepted %s for %s from %s port %s ssh2: %s", client.Config.AuthenticationMethod, conn.User(), remoteAddr[0], remoteAddr[1], client.Config.AuthenticationComment)
	return &client
}

// HandleRequests handles SSH requests
func (c *Client) HandleRequests() error {
	go func(in <-chan *ssh.Request) {
		for req := range in {
			log.Debugf("HandleRequest: %v", req)
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}(c.Reqs)
	return nil
}

// HandleChannels handles SSH channels
func (c *Client) HandleChannels() error {
	for newChannel := range c.Chans {
		if err := c.HandleChannel(newChannel); err != nil {
			return err
		}
	}
	return nil
}

// HandleChannel handles one SSH channel
func (c *Client) HandleChannel(newChannel ssh.NewChannel) error {
	if newChannel.ChannelType() != "session" {
		log.Debugf("Unknown channel type: %s", newChannel.ChannelType())
		newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
		return nil
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		log.Errorf("newChannel.Accept failed: %v", err)
		return err
	}
	c.ChannelIdx++
	log.Debugf("HandleChannel.channel (client=%d channel=%d): %v", c.Idx, c.ChannelIdx, channel)

	log.Debug("Creating pty...")
	f, tty, err := pty.Open()
	if err != nil {
		log.Errorf("pty.Open failed: %v", err)
		return nil
	}
	c.Tty = tty
	c.Pty = f

	c.HandleChannelRequests(channel, requests)

	return nil
}

// HandleChannelRequests handles channel requests
func (c *Client) HandleChannelRequests(channel ssh.Channel, requests <-chan *ssh.Request) {
	go func(in <-chan *ssh.Request) {
		defer c.Tty.Close()

		for req := range in {
			ok := false
			switch req.Type {
			case "shell":
				log.Debugf("HandleChannelRequests.req shell")
				if len(req.Payload) != 0 {
					break
				}
				ok = true

				var cmd *exec.Cmd
				var err error

				if c.Config.IsLocal {
					cmd = exec.Command("/bin/bash")
				} else {
					// checking if a container already exists for this user
					existingContainer := ""
					if !c.Server.NoJoin {
						cmd := exec.Command("docker", "ps", "--filter=label=ssh2docker", fmt.Sprintf("--filter=label=image=%s", c.Config.ImageName), fmt.Sprintf("--filter=label=user=%s", c.Config.RemoteUser), "--quiet", "--no-trunc")
						cmd.Env = c.Config.Env.List()
						buf, err := cmd.CombinedOutput()
						if err != nil {
							log.Warnf("docker ps ... failed: %v", err)
							continue
						}
						existingContainer = strings.TrimSpace(string(buf))
					}

					// Opening Docker process
					if existingContainer != "" {
						// Attaching to an existing container
						shell := c.Server.DefaultShell
						if c.Config.EntryPoint != "" {
							shell = c.Config.EntryPoint
						}
						args := []string{"exec", "-it", existingContainer, shell}
						log.Debugf("Executing 'docker %s'", strings.Join(args, " "))
						cmd = exec.Command("docker", args...)
						cmd.Env = c.Config.Env.List()
					} else {
						// Creating and attaching to a new container
						args := []string{"run"}
						if len(c.Config.DockerRunArgs) > 0 {
							args = append(args, c.Config.DockerRunArgs...)
						} else {
							args = append(args, c.Server.DockerRunArgs...)
						}
						args = append(args, "--label=ssh2docker", fmt.Sprintf("--label=user=%s", c.Config.RemoteUser), fmt.Sprintf("--label=image=%s", c.Config.ImageName))
						if c.Config.User != "" {
							args = append(args, "-u", c.Config.User)
						}
						if c.Config.EntryPoint != "" {
							args = append(args, "--entrypoint", c.Config.EntryPoint)
						}
						args = append(args, c.Config.ImageName)
						if c.Config.Command != nil {
							args = append(args, c.Config.Command...)
						} else {
							args = append(args, c.Server.DefaultShell)
						}
						log.Debugf("Executing 'docker %s'", strings.Join(args, " "))
						cmd = exec.Command("docker", args...)
						cmd.Env = c.Config.Env.List()
					}
				}

				if c.Server.Banner != "" {
					banner := c.Server.Banner
					banner = strings.Replace(banner, "\r", "", -1)
					banner = strings.Replace(banner, "\n", "\n\r", -1)
					fmt.Fprintf(channel, "%s\n\r", banner)
				}

				cmd.Stdout = c.Tty
				cmd.Stdin = c.Tty
				cmd.Stderr = c.Tty
				cmd.SysProcAttr = &syscall.SysProcAttr{
					Setctty: true,
					Setsid:  true,
				}

				err = cmd.Start()
				if err != nil {
					log.Warnf("cmd.Start failed: %v", err)
					continue
				}

				var once sync.Once
				close := func() {
					channel.Close()
					log.Infof("Received disconnect from %s: disconnected by user", c.ClientID)
				}

				go func() {
					io.Copy(channel, c.Pty)
					once.Do(close)
				}()

				go func() {
					io.Copy(c.Pty, channel)
					once.Do(close)
				}()

				go func() {
					if err := cmd.Wait(); err != nil {
						log.Warnf("cmd.Wait failed: %v", err)
					}
					once.Do(close)
				}()

			case "exec":
				command := string(req.Payload)
				log.Debugf("HandleChannelRequests.req exec: %q", command)
				ok = false

				fmt.Fprintln(channel, "⚠️  ssh2docker: exec is not yet implemented. https://github.com/moul/ssh2docker/issues/51.")
				time.Sleep(3 * time.Second)

			case "pty-req":
				ok = true
				termLen := req.Payload[3]
				c.Config.Env["TERM"] = string(req.Payload[4 : termLen+4])
				w, h := ttyhelper.ParseDims(req.Payload[termLen+4:])
				ttyhelper.SetWinsize(c.Pty.Fd(), w, h)
				log.Debugf("HandleChannelRequests.req pty-req: TERM=%q w=%q h=%q", c.Config.Env["TERM"], int(w), int(h))

			case "window-change":
				w, h := ttyhelper.ParseDims(req.Payload)
				ttyhelper.SetWinsize(c.Pty.Fd(), w, h)
				continue

			case "env":
				keyLen := req.Payload[3]
				key := string(req.Payload[4 : keyLen+4])
				valueLen := req.Payload[keyLen+7]
				value := string(req.Payload[keyLen+8 : keyLen+8+valueLen])
				log.Debugf("HandleChannelRequets.req 'env': %s=%q", key, value)
				c.Config.Env[key] = value

			default:
				log.Debugf("Unhandled request type: %q: %v", req.Type, req)
			}

			if req.WantReply {
				if !ok {
					log.Debugf("Declining %s request...", req.Type)
				}
				req.Reply(ok, nil)
			}
		}
	}(requests)
}
