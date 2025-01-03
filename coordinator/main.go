/*
 * MIT License
 *
 * Copyright (c) 2019-2021 Ecole Polytechnique Federale Lausanne (EPFL)
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */
package main

// #include "../inc/lancet/coord_proto.h"
import "C"
import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	tHROUGHPUT_AGENT = iota
	lATENCY_AGENT
)

type agent struct {
	name  string
	conn  *net.TCPConn
	aType int
}

func main() {

	serverCfg, expCfg, generalCfg, err := ParseConfig()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	c := coordinator{}

	if expCfg.thAgents != nil {
		c.thAgents = make([]*agent, len(expCfg.thAgents))
	}
	if expCfg.ltAgents != nil {
		c.ltAgents = make([]*agent, len(expCfg.ltAgents))
	}
	if expCfg.symAgents != nil {
		c.symAgents = make([]*agent, len(expCfg.symAgents))
	}

	c.agentPort = expCfg.agentPort

	var agentArgsMap map[string]string
	if generalCfg.printAgentArgs {
		agentArgsMap = make(map[string]string)
	}

	// Run throughput agents
	agentArgs := fmt.Sprintf("-s %s -t %d -z %d -c %d -o %d -i %s -p %s -r %s -a 0",
		serverCfg.target, serverCfg.thThreads, serverCfg.affinityBase, serverCfg.thConn, serverCfg.reqPerConn,
		serverCfg.idist, serverCfg.comProto, serverCfg.appProto)
	for i, a := range expCfg.thAgents {
		if generalCfg.printAgentArgs {
			agentArgsMap[a] = agentArgs
		} else if generalCfg.runAgents {
			session, err := runAgent(a, expCfg.privateKeyPath, agentArgs)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			defer session.Close()
		}
		c.thAgents[i] = &agent{name: a, aType: tHROUGHPUT_AGENT}
	}

	// Run latency agents
	ltArgs := fmt.Sprintf("-s %s -t %d -z %d -c %d -i %s -p %s -r %s -a 1 -o 1",
		serverCfg.target, serverCfg.ltThreads, serverCfg.affinityBase, serverCfg.ltConn,
		serverCfg.idist, serverCfg.comProto, serverCfg.appProto)
	for i, a := range expCfg.ltAgents {
		if generalCfg.printAgentArgs {
			agentArgsMap[a] = ltArgs
		} else if generalCfg.runAgents {
			session, err := runAgent(a, expCfg.privateKeyPath, ltArgs)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			defer session.Close()
		}
		c.ltAgents[i] = &agent{name: a, aType: lATENCY_AGENT}
	}

	symArgsPre := fmt.Sprintf("-s %s -t %d -z %d -c %d -o %d -i %s -p %s -r %s",
		serverCfg.target, serverCfg.thThreads, serverCfg.affinityBase, serverCfg.thConn, serverCfg.reqPerConn,
		serverCfg.idist, serverCfg.comProto, serverCfg.appProto)
	var symArgs string
	if expCfg.nicTS {
		if len(serverCfg.ifNames) == 0 {
			fmt.Println("No interfaces given for NIC timestamping")
			os.Exit(1)
		}

		if len(serverCfg.ifNames) > 1 && serverCfg.bindToNIC {
			fmt.Println("Can't bind to multiple NICs (remove -bindToNIC)")
			os.Exit(1)
		}

		ifaceArgs := ""
		for _, iface := range serverCfg.ifNames {
			ifaceArgs = fmt.Sprintf("%s -n %s", ifaceArgs, iface)
		}
		symArgs = fmt.Sprintf("%s -a %d %s", symArgsPre, 2, ifaceArgs)

		if serverCfg.bindToNIC {
			symArgs = fmt.Sprintf("%s -b", symArgs)
		}
	} else {
		symArgs = fmt.Sprintf("%s -a %d", symArgsPre, 3)
	}
	for i, a := range expCfg.symAgents {
		if generalCfg.printAgentArgs {
			agentArgsMap[a] = symArgs
		} else if generalCfg.runAgents {
			session, err := runAgent(a, expCfg.privateKeyPath, symArgs)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			defer session.Close()
		}
		c.symAgents[i] = &agent{name: a, aType: lATENCY_AGENT}
	}

	if generalCfg.printAgentArgs {
		agentArgsMapStr, _ := json.Marshal(agentArgsMap)
		fmt.Println(string(agentArgsMapStr))
		os.Exit(0)
	}

	if generalCfg.runAgents {
		time.Sleep(5000 * time.Millisecond)
	}

	// Initialize management connections
	for _, a := range c.thAgents {
		tcpAddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", a.name, c.agentPort))
		if err != nil {
			fmt.Println("ResolveTCPAddr failed:", err)
			os.Exit(1)

		}
		conn, err := net.DialTCP("tcp", nil, tcpAddr)
		if err != nil {
			fmt.Println("Dial failed:", err)
			os.Exit(1)

		}
		defer conn.Close()
		a.conn = conn
	}
	for _, a := range c.ltAgents {
		tcpAddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", a.name, c.agentPort))
		if err != nil {
			fmt.Println("ResolveTCPAddr failed:", err)
			os.Exit(1)

		}
		conn, err := net.DialTCP("tcp", nil, tcpAddr)
		if err != nil {
			fmt.Println("Dial failed:", err)
			os.Exit(1)

		}
		defer conn.Close()
		a.conn = conn
	}
	for _, a := range c.symAgents {
		tcpAddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", a.name, c.agentPort))
		if err != nil {
			fmt.Println("ResolveTCPAddr failed:", err)
			os.Exit(1)

		}
		conn, err := net.DialTCP("tcp", nil, tcpAddr)
		if err != nil {
			fmt.Println("Dial failed:", err)
			os.Exit(1)

		}
		defer conn.Close()
		a.conn = conn
	}

	// Run experiment
	c.shouldWaitConn = false
	if serverCfg.comProto == "TCP" {
		c.shouldWaitConn = true
	}
	err = c.runExp(expCfg.loadPattern, expCfg.ltRate, expCfg.ciSize)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
