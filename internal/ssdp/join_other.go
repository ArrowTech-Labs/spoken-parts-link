//go:build !linux && !darwin

package ssdp

import (
	"net"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
)

func joinAll(*net.UDPConn, net.IP, *logx.Logger) {}
