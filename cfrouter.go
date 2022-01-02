package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	_ "runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	set "github.com/deckarep/golang-set"
	"github.com/mroth/weightedrand"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var router *SRouter
var p = message.NewPrinter(language.English)

type MeterName int
type BalancerName int

const (
	MeterPerChannel1s MeterName = iota
	MeterPerChannel5s
	MeterPerChannel15s
	MeterPerChannel1m
	MeterPerChannel5m
)
const (
	BalancerBestThroughput BalancerName = iota
	BalancerWeighted
	BalancerWeightedSquare
)

func (n MeterName) String() string {
	switch n {
	case MeterPerChannel1s:
		return "PerChannel1s"
	case MeterPerChannel5s:
		return "PerChannel5s"
	case MeterPerChannel15s:
		return "PerChannel15s"
	case MeterPerChannel1m:
		return "PerChannel1m"
	case MeterPerChannel5m:
		return "PerChannel5m"
	default:
		return fmt.Sprintf("UnknownMeterName %d", n)
	}
}

func (n BalancerName) String() string {
	switch n {
	case BalancerWeighted:
		return "Weighted"
	case BalancerWeightedSquare:
		return "WeightedSquare"
	case BalancerBestThroughput:
		return "BestThroughput"
	default:
		return fmt.Sprintf("UnknownBalancer %d", n)
	}
}

/// Router 是用户应该使用的对象，创建 Router 后，调用 Add 方法往里面添加节点即可
/// 随后，调用 NewChannel() 方法即可获得一个连接到 CF 节点的 IO 对象
type SRouter struct {
	BindAddress string
	Nodes       []*SNode
	NodesMu     sync.Mutex
	UseMeter    MeterName
	UseBalancer BalancerName

	/// 在 32 位系统上，如果尝试对 int64 字段进行 atomic.Add() 操作，而这个 int64 字段在结构体中没有对齐到 64bit 的话，就会抛出一个 panic()
	/// panic: unaligned 64-bit atomic operation
	/// 这个 Dummy 字段就是为了用来保证下面的 int64 类型都对齐到了 64 位上
	DummyPackFor32Bit int32

	/// 下行速率记录变量，由 Meter() 方法每秒钟更新一次。后缀意义同 SNode 中的成员
	SpeedBps1s  int64
	SpeedBps5s  int64
	SpeedBps15s int64
	SpeedBps1m  int64
	SpeedBps5m  int64

	/// 总计已经读取和写入的字节数
	ReadN      int64
	LastReadN  int64
	WriteN     int64
	LastWriteN int64

	/// 总计已经创建的 Channel 数量
	ChannelServed int64

	/// 启动时间
	StartTime time.Time

	/// 在多长时间内，保证节点可以分配到一个连接
	/// 节点失速后，速度可能变得很低，这时候这个节点可能就会永远分配不到连接。为了解决这一问题，可以强制要求多长时间后，对节点分配一个连接
	/// 当 ForeceDistanceNodeDuration 不为 0 时，router 将会每隔 ForceDispatchNodeDuration，从现有节点中，取出一个最后活动时间最长、且活动连接数不为 0 的节点，将连接分配给它
	ForceDispatchNodeDuration time.Duration
	LastForceDispatchTime     time.Time

	/// 节点失速控制
	/// 如果一个节点在连接过后超过 StallThreshold 还没有任何数据收到，则认为这个节点失速了
	/// 失速节点将会被标记出来，并在 StallRecoverDuration 后被重新标记为正常
	/// SelectNodeForceDispatch() 不会选择出失速的节点
	/// 其他 SelectNode*() 方法，将会优先选择未失速的节点
	StallThreshold       time.Duration
	StallRecoverDuration time.Duration
}

/// 一个 CF 节点就是一个 Node，这个结构体记录了 Node 地址以及速度统计信息
/// Target 是 Node 地址，例如 1.0.0.1:443
/// SpeedBps 是下行速率，后缀 1s, 1m, 5m 分别代表 1 秒、1 分钟和 5 分钟均值
/// SpeedBpsPerChannel 是该节点下所有 Channel 的平均下载速率，后缀意义同上
type SNode struct {
	Target                string
	SpeedBps1s            int64
	SpeedBps1m            int64
	SpeedBps5m            int64
	SpeedBpsPerChannel1s  int64
	SpeedBpsPerChannel5s  int64
	SpeedBpsPerChannel15s int64
	SpeedBpsPerChannel1m  int64
	SpeedBpsPerChannel5m  int64

	WriteN int64
	ReadN  int64

	/// 总计已经创建的 Channel 数量
	ChannelServedN int32

	/// 保存这个节点当前拥有的 Channels
	Channels   set.Set
	ChannelsMu sync.Mutex

	QuitCh chan struct{}

	/// 最后一次被选中的时间
	LastSelectedTime time.Time

	/// 最后一次活动的时间（开始连接、有数据传输都视为活动）
	LastActiveTime time.Time

	/// 最后一次收到数据的时间
	LastReadTime time.Time

	/// 正在连接到远程服务器的连接数量
	ConnectingN int32

	/// 是否失速了
	isStalled bool
	/// 失速恢复时间
	StallRecoverAt time.Time
	/// 失速判断的辅助变量：从这个时间点开始就没有收到数据
	NoDataSince time.Time
}

/// Channel 是一个抽象 IO 对象，目的是为了计算下载速率
type SChannel struct {
	io.Reader
	io.Writer

	/// 指向这个 Channel 关联的 Node 对象
	Node *SNode

	ToConn net.Conn
	/// 用于告诉 Meter() 应该退出的通道
	CloseCh chan struct{}

	/// 已经读取和写入的字节总数
	WriteN int64
	ReadN  int64

	/// 统计出来的连接速率(BytePerSecond)
	WriteBps int64
	ReadBps  int64
}

func (s *SRouter) NewChannel() (*SChannel, error) {
	node, err := s.SelectNode()
	if err != nil {
		return nil, err
	}

	ret := SChannel{
		CloseCh: make(chan struct{}, 1),
		Node:    node,
	}

	log.Printf("连接 %s ...", node.Target)

	node.LastActiveTime = time.Now()
	atomic.AddInt32(&node.ConnectingN, 1)
	toAddr, _ := net.ResolveTCPAddr("tcp4", node.Target)
	conn, err := net.DialTCP("tcp4", nil, toAddr)
	atomic.AddInt32(&node.ConnectingN, -1)

	if err != nil {
		err := fmt.Errorf("连接到远程主机失败: %v", err)
		return nil, err
	}

	atomic.AddInt64(&s.ChannelServed, 1)

	node.AddChannel(&ret)

	go ret.Meter()

	ret.ToConn = conn
	return &ret, nil
}

/// 从当前的节点列表中，选择一个最适合的节点
/// 目前会返回 5s 平均 Channel 速度最快的节点
func (s *SRouter) SelectNode() (*SNode, error) {
	s.NodesMu.Lock()
	defer s.NodesMu.Unlock()

	if len(s.Nodes) == 0 {
		return nil, fmt.Errorf("尚未添加任何 CF 节点")
	}

	for _, v := range s.Nodes {
		/// 保证每个节点至少请求了一次，以便测量速度
		if v.ChannelServedN == 0 {
			return v, nil
		}
	}

	/// 开始分配节点
	var ret *SNode
	var err error

	/// 1. 如果设定了至少 n 秒强制使用一个连接数为 0 的节点
	now := time.Now()
	if s.ForceDispatchNodeDuration != 0 {
		if now.Sub(s.LastForceDispatchTime) > s.ForceDispatchNodeDuration {
			ret, err = s.SelectNodeForceDispatch()
			s.LastForceDispatchTime = now
		}
	}
	if err != nil {
		log.Printf("无法强制分配节点: %v", err)
	}
	if ret != nil {
		return ret, nil
	}

	/// 2. 按照设定的 balancer 进行分配
	switch s.UseBalancer {
	case BalancerBestThroughput:
		ret, err = s.SelectNodeBestThroughput()
	case BalancerWeighted:
		ret, err = s.SelectNodeWeighted()
	case BalancerWeightedSquare:
		ret, err = s.SelectNodeWeightedSquare()
	default:
		panic("该情况尚未处理，请向开发者报告错误")
	}

	if err != nil {
		return nil, err
	}

	ret.LastSelectedTime = time.Now()

	return ret, nil
}

/// 以 5s 通道平均下载速度作为权重，按权重选择节点。速度越快的节点越容易被选中
func (s *SRouter) SelectNodeWeighted() (*SNode, error) {
	/// 按权重进行选择，但只选择没有失速的节点
	var choices []weightedrand.Choice
	for _, v := range s.Nodes {
		if v.IsStalled() {
			continue
		}
		p := v.GetMeterValue()
		p += 1
		choices = append(choices, weightedrand.Choice{Item: v, Weight: uint(p)})
	}
	/// 如果不存在不失速的节点（所有节点都失速了），就只好重新全部添加一次
	if len(choices) == 0 {
		for _, v := range s.Nodes {
			p := v.GetMeterValue()
			p += 1
			choices = append(choices, weightedrand.Choice{Item: v, Weight: uint(p)})
		}
	}

	chooser, err := weightedrand.NewChooser(choices...)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	node := chooser.Pick()
	if node == nil {
		return nil, fmt.Errorf("内部错误：选择 weightedrand 失败")
	}

	return node.(*SNode), nil
}

/// 以 5s 通道平均下载速度的平方作为权重，按权重选择节点。速度越快的节点越容易被选中
func (s *SRouter) SelectNodeWeightedSquare() (*SNode, error) {
	/// 按权重进行选择，但只选择没有失速的节点
	var choices []weightedrand.Choice
	for _, v := range s.Nodes {
		if v.IsStalled() {
			continue
		}
		p := v.GetMeterValue()
		p = (p / 1000) * (p / 1000)
		p += 1
		choices = append(choices, weightedrand.Choice{Item: v, Weight: uint(p)})
	}
	/// 如果没有不失速的节点（所有节点都失速了），就只好重新将所有节点添加进来
	for _, v := range s.Nodes {
		p := v.GetMeterValue()
		p = (p / 1000) * (p / 1000)
		p += 1
		choices = append(choices, weightedrand.Choice{Item: v, Weight: uint(p)})
	}

	chooser, err := weightedrand.NewChooser(choices...)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	node := chooser.Pick()
	if node == nil {
		return nil, fmt.Errorf("内部错误：选择 weightedrand 失败")
	}

	return node.(*SNode), nil
}

/// 根据 5s 通道平均下载速率，选择最快的节点
/// 同时保证每个节点至少有一个通道正在运行，以便测量速度
func (s *SRouter) SelectNodeBestThroughput() (*SNode, error) {
	///保证每个节点至少有一个请求的排序
	sort.Slice(s.Nodes, func(a, b int) bool {
		l := s.Nodes[a].GetMeterValue() > s.Nodes[b].GetMeterValue()
		return l
	})

	var retNotStalled *SNode
	var retStalled *SNode
	var MaxSpeed int64 = -1
	//ts := time.Now()
	for _, v := range s.Nodes {
		if v.GetMeterValue() > MaxSpeed {
			retStalled = v
			if v.IsStalled() {
				/// 节点失速了
			} else {
				retNotStalled = v
			}

			MaxSpeed = v.GetMeterValue()
		}
	}

	if retNotStalled != nil {
		return retNotStalled, nil
	}
	return retStalled, nil
}

/// 根据 ForceDispatchNodeDuration 强制分配一个节点
/// 选取规则：
/// 	1. （已废弃）仅会选出 ActiveChannel 数量为 0 的节点进行强制分配
/// 	2. 选取 LastActiveTime 最旧的节点
func (s *SRouter) SelectNodeForceDispatch() (*SNode, error) {
	/// 1. 选出 ActiveChannel 为 0 的节点
	// var candi []*SNode
	// for _, v := range s.Nodes {
	// 	if v.Channels.Cardinality() == 0 {
	// 		candi = append(candi, v)
	// 	}
	// }

	// /// 如果没有通道数为 0 的节点，则直接返回空
	// if len(candi) == 0 {
	// 	return nil, nil
	// }

	candi := s.Nodes

	/// 2. 按照 LastActiveTime 进行排序，同时将失速的节点排在最后
	sort.Slice(candi, func(a, b int) bool {
		aIsStalled := candi[a].IsStalled()
		bIsStalled := candi[b].IsStalled()
		if aIsStalled == true && bIsStalled == false {
			return false
		} else if aIsStalled == false && bIsStalled == true {
			return true
		}

		l := (candi[a].LastActiveTime.Sub(candi[b].LastActiveTime) < 0)
		return l
	})

	/// 3. 选出第一个没有失速的节点
	for _, v := range candi {
		if v.LastActiveTime.Sub(v.LastReadTime) < s.StallThreshold {
			return v, nil
		}
	}

	/// 4. 没有选择到任何节点，直接返回空
	return nil, nil
}

func NewNode(target string) *SNode {
	node := SNode{
		Channels: set.NewSet(),
		Target:   target,
		QuitCh:   make(chan struct{}, 1),
	}

	go node.Meter()

	return &node
}

func (s *SNode) Destroy() {
	s.QuitCh <- struct{}{}
}

func (s *SNode) AddChannel(c *SChannel) {
	s.ChannelsMu.Lock()
	defer s.ChannelsMu.Unlock()

	atomic.AddInt32(&s.ChannelServedN, 1)
	s.Channels.Add(c)
}

func (s *SNode) RemoveChannel(c *SChannel) {
	s.ChannelsMu.Lock()
	defer s.ChannelsMu.Unlock()

	s.Channels.Remove(c)
}

func (s *SNode) IsStalled() bool {
	//return s.LastActiveTime.Sub(s.LastReadTime) > router.StallThreshold
	return s.isStalled
}

func (s *SNode) CheckStall() {
	/// 如果在失速判断时间内收到了数据，则直接认为没有失速，并清除失速标志
	isNoData := s.LastActiveTime.Sub(s.LastReadTime) > router.StallThreshold
	if isNoData == false {
		s.isStalled = false
		s.NoDataSince = time.Time{}
		return
	}

	/// 当前已经处于失速状态，检查一下是否需要恢复，然后直接返回
	if s.isStalled {
		if time.Now().Sub(s.StallRecoverAt) > 0 {
			s.isStalled = false
			s.NoDataSince = time.Time{}
		}
		return
	}

	/// 如果没有收到数据，且之前没有设定过 noDataSince，则设定一个新的 noDataSince
	if isNoData {
		if s.NoDataSince.IsZero() {
			s.NoDataSince = time.Now()
		}
	}

	/// 在失速判断时间超过后仍没有收到数据，就认为失速了
	if isNoData {
		if time.Now().Sub(s.NoDataSince) > router.StallThreshold {
			s.isStalled = true
			s.StallRecoverAt = time.Now().Add(router.StallRecoverDuration)
		}
	}
}

func (s *SNode) Meter() {
	t := time.NewTicker(1 * time.Second)

	for {
		select {
		case <-t.C:
			/// 计算 SpeedBpsPerChannel
			var bps int64
			var n int64
			it := s.Channels.Iter()
			for v := range it {
				bps += v.(*SChannel).ReadBps
				n++
			}

			if bps > 0 {
				s.LastActiveTime = time.Now()
				s.LastReadTime = time.Now()
			}

			if bps > 0 {
				if n != 0 {
					s.SpeedBpsPerChannel1s = bps / n
				}

				s.SpeedBpsPerChannel5s = int64(float64(s.SpeedBpsPerChannel1s)*0.3333333 + float64(s.SpeedBpsPerChannel5s)*(1-0.3333333))
				s.SpeedBpsPerChannel15s = int64(float64(s.SpeedBpsPerChannel1s)*0.125 + float64(s.SpeedBpsPerChannel15s)*(1-0.125))
				s.SpeedBpsPerChannel1m = int64(float64(s.SpeedBpsPerChannel1s)*0.03278689 + float64(s.SpeedBpsPerChannel1m)*(1-0.03278689))
				s.SpeedBpsPerChannel5m = int64(float64(s.SpeedBpsPerChannel1s)*0.006644518 + float64(s.SpeedBpsPerChannel5m)*(1-0.006644518))
			}

			if bps > 0 {
				//				log.Printf("%s 平均连接速度是 %0.3fMbps", s.Target, float64(s.SpeedBpsPerChannel1s)*8/1024/1024)
			}

			s.CheckStall()

		case <-s.QuitCh:
			return
		}
	}
}

func (s *SNode) GetMeterValue() int64 {
	switch router.UseMeter {
	case MeterPerChannel1s:
		return s.SpeedBpsPerChannel1s
	case MeterPerChannel5s:
		return s.SpeedBpsPerChannel5s
	case MeterPerChannel15s:
		return s.SpeedBpsPerChannel15s
	case MeterPerChannel1m:
		return s.SpeedBpsPerChannel1m
	case MeterPerChannel5m:
		return s.SpeedBpsPerChannel5m
	default:
		panic("未处理的情况，请向开发者报告错误")
	}
}

func (s *SChannel) Close() {
	s.ToConn.Close()
	s.CloseCh <- struct{}{}
	s.Node.RemoveChannel(s)
}

func (s *SChannel) Write(p []byte) (n int, err error) {
	//	log.Printf("写入了 %d 个字节", len(p))

	//stime := time.Now()
	n, err = s.ToConn.Write(p)

	atomic.AddInt64(&s.WriteN, int64(n))
	atomic.AddInt64(&s.Node.WriteN, int64(n))
	atomic.AddInt64(&router.ReadN, int64(n))

	//	log.Printf("写入完毕，%d 字节，耗时 %v", n, time.Now().Sub(stime))
	return n, err
}

func (s *SChannel) Read(p []byte) (n int, err error) {
	//	log.Printf("即将读取 %d 个字节", 0)

	//ts := time.Now()
	n, err = s.ToConn.Read(p)

	atomic.AddInt64(&s.ReadN, int64(n))
	atomic.AddInt64(&s.Node.ReadN, int64(n))
	atomic.AddInt64(&router.ReadN, int64(n))

	//	log.Printf("读取完毕了，%d 个字节，耗时 %v", n, time.Now().Sub(ts))
	return n, err
}

/// 用于统计下载速度的协程，每秒钟会计算一次速度
func (s *SChannel) Meter() {
	var LastWriteN int64
	var LastReadN int64

	t := time.NewTicker(1 * time.Second)

	for {
		select {
		case <-t.C:
			/// 什么都不用做暂时
			WriteN := s.WriteN - LastWriteN
			ReadN := s.ReadN - LastReadN

			LastWriteN = s.WriteN
			LastReadN = s.ReadN

			s.WriteBps = WriteN
			s.ReadBps = ReadN

			WriteMbps := float64(WriteN) / 1024 / 1024 * 8
			ReadMbps := float64(ReadN) / 1024 / 1024 * 8

			if WriteMbps > 0 || ReadMbps > 0 {
				//				log.Printf("Write 速度: %0.3fMbps, Read 速度 %0.3fMbps", WriteMbps, ReadMbps)
			}

			s.Node.LastSelectedTime = time.Now()
		case <-s.CloseCh:
			return
		}
	}
}

func main() {
	/// 读取环境变量配置
	ChannelBind := os.Getenv("CF_BIND")
	AdminBind := os.Getenv("CF_ADMIN_BIND")

	log.Printf("读取环境变量: CF_BIND=%s", ChannelBind)
	log.Printf("读取环境变量: CF_ADMIN_BIND=%s", AdminBind)

	if ChannelBind == "" {
		ChannelBind = "127.0.0.1:5530"
	}
	if AdminBind == "" {
		AdminBind = "127.0.0.1:5531"
	}

	/// 启动 router
	router = &SRouter{
		BindAddress:               ChannelBind,
		StartTime:                 time.Now(),
		UseBalancer:               BalancerBestThroughput,
		UseMeter:                  MeterPerChannel5s,
		ForceDispatchNodeDuration: 300 * time.Second,
		StallThreshold:            60 * time.Second,
		StallRecoverDuration:      3600 * time.Second,
	}

	/// 从配置文件中读取 Targets
	targetRegExp := regexp.MustCompile(`[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}:[0-9]{1,5}`)
	targets_raw, err := ioutil.ReadFile("cf_targets.txt")
	if err != nil {
		log.Printf("无法打开服务器列表文件 cf_targets.txt，将使用默认的配置: %v", err)
		log.Printf("请在当前目录下创建 cf_targets.txt 文件，文件内容为要使用的服务器地址，每行一个服务器，格式为 IP:PORT")
	}
	targets := strings.Split(string(targets_raw), "\n")
	for _, target := range targets {
		if targetRegExp.MatchString(target) == false {
			continue
		}
		router.Add(target)
	}
	if len(router.Nodes) == 0 {
		log.Printf("未能加载任何远程节点，将添加 Cloudflare 的 DoH 节点作为演示")
		router.Add("1.0.0.1:443")
		router.Add("1.1.1.1:443")
	}

	go router.Start()

	/// 启动监控页面
	log.Printf("管理页面位于 http://%s", AdminBind)
	http.HandleFunc("/api/stat", webApiStat)
	http.HandleFunc("/", webIndex)
	http.HandleFunc("/set_meter", webSetMeter)
	http.HandleFunc("/set_balancer", webSetBalancer)
	http.HandleFunc("/set_force_dispatch_node_duration", webSetForceDispatchNodeDuration)

	err = http.ListenAndServe(AdminBind, nil)
	if err != nil {
		log.Printf("____vvvvvvvvvv____")
		log.Printf("> 初始化管理页面失败：%v", err)
		log.Printf("> 未能初始化管理页面，请使用 CF_ADMIN_BIND 环境变量来设定管理页面的监听地址")
		log.Printf("> 例如：")
		log.Printf("> export CF_ADMIN_BIND=127.0.0.1:23456")
		log.Printf("----^^^^^^^^^^----")
	}

	select {}
}

func (s *SRouter) Add(target string) {
	log.Printf("添加远程节点 %s", target)

	node := NewNode(target)

	s.NodesMu.Lock()
	s.NodesMu.Unlock()

	s.Nodes = append(s.Nodes, node)
}

func (s *SRouter) SetMeter(meter string) error {
	i := []MeterName{MeterPerChannel1s, MeterPerChannel5s, MeterPerChannel15s, MeterPerChannel1m, MeterPerChannel5m}

	meter = strings.ToLower(meter)

	for _, v := range i {
		if strings.ToLower(v.String()) == meter {
			s.UseMeter = v
			return nil
		}
	}

	return fmt.Errorf("找不到名为 `%s' 的负载均衡指标", meter)
}

func (s *SRouter) SetBalancer(balancer string) error {
	i := []BalancerName{BalancerWeighted, BalancerWeightedSquare, BalancerBestThroughput}

	balancer = strings.ToLower(balancer)

	for _, v := range i {
		if strings.ToLower(v.String()) == balancer {
			s.UseBalancer = v
			return nil
		}
	}

	return fmt.Errorf("找不到名为 `%s' 的负载均衡器", balancer)
}

func (s *SRouter) SetForceDispatchNodeDuration(d time.Duration) error {
	s.ForceDispatchNodeDuration = d
	return nil
}

func (s *SRouter) Dump() string {
	s.NodesMu.Lock()
	defer s.NodesMu.Unlock()

	lines := []string{}

	s1 := float64(s.SpeedBps1s) * 8 / 1024 / 1024
	s2 := float64(s.SpeedBps5s) * 8 / 1024 / 1024
	s3 := float64(s.SpeedBps15s) * 8 / 1024 / 1024
	s4 := float64(s.SpeedBps1m) * 8 / 1024 / 1024
	s5 := float64(s.SpeedBps5m) * 8 / 1024 / 1024

	channelN := 0
	for _, v := range s.Nodes {
		channelN += v.Channels.Cardinality()
	}

	now := time.Now()
	lines = append(lines, "=== Stat ===")
	lines = append(lines, fmt.Sprintf("Time:           %v", now))
	lines = append(lines, fmt.Sprintf("Uptime:         %v", now.Sub(router.StartTime)))
	lines = append(lines, p.Sprintf("ActiveChannel:  %d", channelN))
	lines = append(lines, p.Sprintf("ChannelServed:  %d", s.ChannelServed))
	lines = append(lines, fmt.Sprintf("TransferedSize: %s", p.Sprintf("%d", s.ReadN)))
	lines = append(lines, fmt.Sprintf("Speed[1s/5s/15s/1m/5m](Mbps): %7.3fMbps/%7.3fMbps/%7.3fMbps/%7.3fMbps/%7.3fMbps", s1, s2, s3, s4, s5))

	lines = append(lines, "=== Nodes ===")
	//lines = append(lines, "Target \t\t\t SpeedPerChannel[1s/5s/1m/5m] \t\t\t SpeedPerChannel5s \t  SpeedPerChannel1m \t SpeedPerChannel5m \t ChannelCount \t ChannelServed \t\t ReadSize")
	lines = append(lines, "Target [flags]             SpeedPerChannel[1s/5s/15s/1m/5m] \t\t\t      ChannelActive      ChannelServed      TransferdSize      LastActiveTime")

	var nodes []*SNode
	nodes = append(nodes, s.Nodes...)
	sort.Slice(nodes, func(a, b int) bool {
		return strings.Compare(nodes[a].Target, nodes[b].Target) < 0
	})

	for _, v := range nodes {
		// v1 := float64(v.SpeedBpsPerChannel1s) * 8 / 1024 / 1024
		// v2 := float64(v.SpeedBpsPerChannel5s) * 8 / 1024 / 1024
		// v3 := float64(v.SpeedBpsPerChannel15s) * 8 / 1024 / 1024
		// v4 := float64(v.SpeedBpsPerChannel1m) * 8 / 1024 / 1024
		// v5 := float64(v.SpeedBpsPerChannel5m) * 8 / 1024 / 1024

		//lines = append(lines, p.Sprintf("%20s \t %0.3fMbps \t\t %0.3fMbps \t\t %0.3fMbps \t\t %0.3fMbps \t %12d \t %12d \t %15d", v.Target, v1, v2, v3, v4, v.Channels.Cardinality(), v.ChannelServedN, v.ReadN))
		///lines = append(lines, p.Sprintf("%20s \t %7.3fMbps/%7.3fMbps/%7.3fMbps/%7.3fMbps/%7.3fMbps  %12d \t %12d \t %15d", v.Target, v1, v2, v3, v4, v5, v.Channels.Cardinality(), v.ChannelServedN, v.ReadN))

		i1 := v.SpeedBpsPerChannel1s * 8 / 1024
		i2 := v.SpeedBpsPerChannel5s * 8 / 1024
		i3 := v.SpeedBpsPerChannel15s * 8 / 1024
		i4 := v.SpeedBpsPerChannel1m * 8 / 1024
		i5 := v.SpeedBpsPerChannel5m * 8 / 1024

		lastActiveTime := "N/A"
		if v.LastActiveTime.IsZero() == false {
			//lastActiveTime = fmt.Sprintf("%s", now.Sub(v.LastActiveTime))
			lastActiveTime = p.Sprintf("%ds", now.Sub(v.LastActiveTime)/time.Second)
		}

		flags := " "
		if v.IsStalled() {
			flags += "S"
		}

		lines = append(lines, p.Sprintf("%19s %-3s %7dKbps/%7dKbps/%7dKbps/%7dKbps/%7dKbps  %12d \t %12d \t %15d  %15v",
			v.Target, flags, i1, i2, i3, i4, i5, v.Channels.Cardinality(), v.ChannelServedN, v.ReadN, lastActiveTime))
	}

	lines = append(lines, "=== Balancer ===")
	lines = append(lines, fmt.Sprintf("Balancer: %s", s.UseBalancer.String()))
	lines = append(lines, fmt.Sprintf("Meter:    %s", s.UseMeter.String()))
	lines = append(lines, fmt.Sprintf("IsForceDispatchInterval:    %v", s.ForceDispatchNodeDuration != 0))
	lines = append(lines, fmt.Sprintf("ForceDispatchInterval:      %v", s.ForceDispatchNodeDuration))
	lines = append(lines, fmt.Sprintf("StallThreshold:             %v", s.StallThreshold))
	lines = append(lines, fmt.Sprintf("StallRecoverDuration:       %v", s.StallRecoverDuration))

	return strings.Join(lines, "\n")
}

func (s *SRouter) Meter() {
	t := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-t.C:
			bps := (s.ReadN - s.LastReadN)
			s.LastReadN = s.ReadN

			s.SpeedBps1s = bps
			s.SpeedBps5s = int64(float64(s.SpeedBps1s)*0.3333333 + float64(s.SpeedBps5s)*(1-0.3333333))
			s.SpeedBps15s = int64(float64(s.SpeedBps1s)*0.125 + float64(s.SpeedBps15s)*(1-0.125))
			s.SpeedBps1m = int64(float64(s.SpeedBps1s)*0.03278689 + float64(s.SpeedBps1m)*(1-0.03278689))
			s.SpeedBps5m = int64(float64(s.SpeedBps1s)*0.006644518 + float64(s.SpeedBps5m)*(1-0.006644518))
		}

	}
}

func (s *SRouter) Start() {
	/// 1. 监听本地端口
	l, err := net.Listen("tcp", s.BindAddress)
	if err != nil {
		panic(err)
	}

	/// 2. 初始化统计程序
	go s.Meter()

	/// 3. 开始处理请求

	BaseChannelID := 0

	log.Printf("使用负载均衡器: %s", s.UseBalancer.String())
	log.Printf("使用负载均衡器指标: %s", s.UseMeter.String())
	log.Printf("开始监听请求 %s", s.BindAddress)

	for {
		BaseChannelID++
		ChannelID := BaseChannelID

		conn, err := l.Accept()
		if err != nil {
			log.Printf("#%d Accept() 错误: %v", ChannelID, err)
			continue
		}
		log.Printf("#%d 收到一个连接请求", ChannelID)

		go func() {
			channel, err := s.NewChannel()
			if err != nil {
				log.Printf("#%d 无法创建通道: %v", ChannelID, err)
				conn.Close()
				return
			}

			log.Printf("#%d 开始进行转发", ChannelID)
			go func() {
				var wg sync.WaitGroup
				wg.Add(2)

				go func() {
					io.Copy(conn, channel)
					wg.Done()
				}()
				go func() {
					io.Copy(channel, conn)
					wg.Done()
				}()

				wg.Wait()

				conn.Close()
				channel.Close()

				log.Printf("#%d 转发完成了", ChannelID)
			}()
		}()

	}
}
