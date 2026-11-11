package host_test

import (
	"context"
	"fmt"

	"github.com/inference-sh/shigawire/host"
)

// A unit written, systemd reloaded and the service restarted when the
// unit changed, then the service enabled and running. Run twice, the
// second run changes nothing.
func Example() {
	x := host.OSExec{Path: host.RootPath}
	apt := &host.Apt{Exec: x}
	restart := &host.Handler{Name: "restart nginx", Resource: &host.Restart{Exec: x, Name: "nginx"}}
	play := host.Play{
		Tasks: []host.Task{
			{Name: "the package lists", Resource: apt.Update(0)},
			{Name: "nginx", Resource: apt.Install("nginx")},
			{Name: "the site", Notify: []*host.Handler{restart}, Resource: &host.File{
				Path: "/etc/nginx/conf.d/site.conf", Content: []byte("server { listen 8080; }\n"), Mode: 0o644}},
			{Name: "nginx runs", Resource: &host.Service{Exec: x, Name: "nginx", Enabled: true, Running: true}},
		},
		Handlers: []*host.Handler{restart},
	}
	r := &host.Runner{OnResult: func(r host.Result) { fmt.Printf("%s: %s %s\n", r.Status, r.Name, r.Msg) }}
	if err := r.Run(context.Background(), play); err != nil {
		fmt.Println(err)
	}
}
