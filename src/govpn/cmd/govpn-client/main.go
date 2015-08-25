/*
GoVPN -- simple secure free software virtual private network daemon
Copyright (C) 2014-2015 Sergey Matveev <stargrave@stargrave.org>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

// Simple secure free software virtual private network daemon client.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"time"

	"govpn"
)

var (
	remoteAddr = flag.String("remote", "", "Remote server address")
	proto      = flag.String("proto", "udp", "Protocol to use: udp or tcp")
	ifaceName  = flag.String("iface", "tap0", "TAP network interface")
	IDRaw      = flag.String("id", "", "Client identification")
	keyPath    = flag.String("key", "", "Path to passphrase file")
	upPath     = flag.String("up", "", "Path to up-script")
	downPath   = flag.String("down", "", "Path to down-script")
	stats      = flag.String("stats", "", "Enable stats retrieving on host:port")
	proxyAddr  = flag.String("proxy", "", "Use HTTP proxy on host:port")
	proxyAuth  = flag.String("proxy-auth", "", "user:password Basic proxy auth")
	mtu        = flag.Int("mtu", 1452, "MTU for outgoing packets")
	timeoutP   = flag.Int("timeout", 60, "Timeout seconds")
	noisy      = flag.Bool("noise", false, "Enable noise appending")
	cpr        = flag.Int("cpr", 0, "Enable constant KiB/sec out traffic rate")
	egdPath    = flag.String("egd", "", "Optional path to EGD socket")
)

func main() {
	flag.Parse()
	timeout := *timeoutP
	var err error
	log.SetFlags(log.Ldate | log.Lmicroseconds | log.Lshortfile)

	govpn.MTU = *mtu

	id, err := govpn.IDDecode(*IDRaw)
	if err != nil {
		log.Fatalln(err)
	}

	if *egdPath != "" {
		log.Println("Using", *egdPath, "EGD")
		govpn.EGDInit(*egdPath)
	}

	pub, priv := govpn.NewVerifier(id, govpn.StringFromFile(*keyPath))
	conf := &govpn.PeerConf{
		Id:          id,
		Timeout:     time.Second * time.Duration(timeout),
		NoiseEnable: *noisy,
		CPR:         *cpr,
		DSAPub:      pub,
		DSAPriv:     priv,
	}
	govpn.PeersInitDummy(id, conf)

	var conn govpn.RemoteConn
	var sink chan []byte
	var ready chan struct{}
	switch *proto {
	case "udp":
		conn, sink, ready = startUDP()
	case "tcp":
		if *proxyAddr != "" {
			conn, sink, ready = proxyTCP()
		} else {
			conn, sink, ready = startTCP()
		}
	default:
		log.Fatalln("Unknown protocol specified")
	}

	tap, ethSink, ethReady, _, err := govpn.TAPListen(
		*ifaceName,
		time.Second*time.Duration(timeout),
		*cpr,
	)
	if err != nil {
		log.Fatalln("Can not listen on TAP interface:", err)
	}

	timeouts := 0
	firstUpCall := true
	var peer *govpn.Peer
	var ethPkt []byte
	var pkt []byte
	knownPeers := govpn.KnownPeers(map[string]**govpn.Peer{*remoteAddr: &peer})

	log.Println(govpn.VersionGet())
	log.Println("Connected to", *proto, *remoteAddr)
	log.Println("Max MTU on TAP interface:", govpn.TAPMaxMTU())
	if *stats != "" {
		log.Println("Stats are going to listen on", *stats)
		statsPort, err := net.Listen("tcp", *stats)
		if err != nil {
			log.Fatalln("Can not listen on stats port:", err)
		}
		go govpn.StatsProcessor(statsPort, &knownPeers)
	}

	termSignal := make(chan os.Signal, 1)
	signal.Notify(termSignal, os.Interrupt, os.Kill)

	log.Println("Starting handshake")
	handshake := govpn.HandshakeStart(*remoteAddr, conn, conf)

MainCycle:
	for {
		if peer != nil && (peer.BytesIn+peer.BytesOut) > govpn.MaxBytesPerKey {
			peer.Zero()
			peer = nil
			handshake = govpn.HandshakeStart(*remoteAddr, conn, conf)
			log.Println("Rehandshaking")
		}
		select {
		case <-termSignal:
			break MainCycle
		case ethPkt = <-ethSink:
			if peer == nil {
				if len(ethPkt) > 0 {
					ethReady <- struct{}{}
				}
				continue
			}
			peer.EthProcess(ethPkt, ethReady)
		case pkt = <-sink:
			timeouts++
			if timeouts >= timeout {
				break MainCycle
			}
			if pkt == nil {
				ready <- struct{}{}
				continue
			}

			if peer == nil {
				if govpn.IDsCache.Find(pkt) == nil {
					log.Println("Invalid identity in handshake packet")
					ready <- struct{}{}
					continue
				}
				if p := handshake.Client(pkt); p != nil {
					log.Println("Handshake completed")
					if firstUpCall {
						go govpn.ScriptCall(*upPath, *ifaceName)
						firstUpCall = false
					}
					peer = p
					handshake.Zero()
					handshake = nil
				}
				ready <- struct{}{}
				continue
			}
			if peer == nil {
				ready <- struct{}{}
				continue
			}
			if peer.PktProcess(pkt, tap, ready) {
				timeouts = 0
			}
		}
	}
	govpn.ScriptCall(*downPath, *ifaceName)
}
