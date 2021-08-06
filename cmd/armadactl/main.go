package main

import (
	"github.com/G-Research/armada/cmd/armadactl/cmd"
	"github.com/G-Research/armada/cmd/armadactl/cmd/create"
	"github.com/G-Research/armada/internal/common"
)

func main() {
	create.InitCreate()
	common.ConfigureCommandLineLogging()
	cmd.Execute()
}
