module gobgp-evpn-agent

go 1.21.3

require (
	github.com/osrg/gobgp/v3 v3.28.0
	github.com/vishvananda/netlink v1.3.1
	google.golang.org/grpc v1.56.3
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/net v0.23.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230731193218-e0aa005b6bdf // indirect
	google.golang.org/protobuf v1.33.0 // indirect
)

replace golang.org/x/net => golang.org/x/net v0.23.0

replace golang.org/x/sys => golang.org/x/sys v0.20.0

replace golang.org/x/text => golang.org/x/text v0.14.0

replace google.golang.org/genproto/googleapis/rpc => google.golang.org/genproto/googleapis/rpc v0.0.0-20230731193218-e0aa005b6bdf

replace google.golang.org/protobuf => google.golang.org/protobuf v1.33.0
