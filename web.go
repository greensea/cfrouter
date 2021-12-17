package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func webApiStat(rw http.ResponseWriter, r *http.Request) {
	rw.Write([]byte(router.Dump()))
}

func webIndex(rw http.ResponseWriter, r *http.Request) {
	rw.Write([]byte(`
<html>
<head>
<title>CFRouter 监控页面</title>
</head>
<style type="text/css">
form {
	display: inline-flex;
}
</style>
<body>
<div id="error" style="width: 99%; text-align: center; position: absolute; color: #f00; text-shadow: 0 0 20px #f00; font-weight: bold;"></div>
<pre id="status" style="width: 99%; height: auto; font-size: large;">正在加载...</pre>

<hr />

<div>
	<form action="/set_balancer" method="post">
		<button>负载均衡器：使用 BestThroughput</button>
		<input type="hidden" name="balancer" value="BestThroughput" />
		<div>将连接分配到连接平均速度最快的节点上</div>
	</form>
</div>
<div>
	<form action="/set_balancer" method="post">
		<button>负载均衡器：使用 Weighted</button>
		<input type="hidden" name="balancer" value="Weighted" />
		<div>以节点的连接平均下载速度为权重，将连接按权重分配到节点上，速度越快权重越高</div>
	</form>
</div>
<div>
	<form action="/set_balancer" method="post">
		<button>负载均衡器：使用 WeightedSquare </button>
		<input type="hidden" name="balancer" value="WeightedSquare" />
		<div>以节点的连接平均下载速度的平方为权重，将连接按权重分配到节点上，速度越快权重越高</div>
	</form>
</div>

<div>
	<form action="/set_meter" method="post">
		<button>使用 PerChannel1s 作为负载均衡器的速度指标</button>
		<input type="hidden" name="meter" value="PerChannel1s" />
	</form>
	<form action="/set_meter" method="post">
		<button>使用 PerChannel5s 作为负载均衡器的速度指标</button>
		<input type="hidden" name="meter" value="PerChannel5s" />
	</form>
	<form action="/set_meter" method="post">
		<button>使用 PerChannel15s 作为负载均衡器的速度指标</button>
		<input type="hidden" name="meter" value="PerChannel15s" />
	</form>
	<form action="/set_meter" method="post">
		<button>使用 PerChannel1m 作为负载均衡器的速度指标</button>
		<input type="hidden" name="meter" value="PerChannel1m" />
	</form>
	<form action="/set_meter" method="post">
		<button>使用 PerChannel5m 作为负载均衡器的速度指标</button>
		<input type="hidden" name="meter" value="PerChannel5m" />
	</form>
</div>


<div>
	<form action="/set_force_dispatch_node_duration" method="post">
		每隔 
		<select name="duration">
			<option value="60">1 分钟</option>
			<option value="300">5 分钟</option>
			<option value="900">15 分钟</option>
			<option value="3600">1 小时</option>
			<option value="7200">2 小时</option>
			<option value="14400">4 小时</option>
			<option value="43200">12 小时</option>
			<option value="86400">24 小时</option>
			<option value="0">= 关闭此功能 =</option>
		</select>
		就强制选择一个连接数为 0 的节点进行连接
		<button>确认</button>
	</form>
</div>

<hr />
<div style="white-space: pre; font-family: monospace; font-size: large;">
# 名词释义

flags: 
	S = 失速（StallThreshold 时长内没有收到任何数据）
SpeedPerChannel: 	一个节点内，所有连接的平均下载速度
ChannelServed:   	总计已经处理了多少个连接
TransferdSize: 		总计已经接收了多少字节的数据
StallThreshold: 	如果一个节点超过 StallThreshold 没有接收到任何数据，则认为这个节点已经死了
StallRecoverTime:   一个节点死掉之后，在 StallRecoverTime 后将会重新激活
</div>

<hr />
<a href="https://github.com/greensea/cfrouter">在 github 上查看 cfrouter 的源码</a>

<script tpe="text/javascript">
setInterval(function() {
		fetch("/api/stat").then(r => r.text()).then(d => {
			document.getElementById("status").innerText = d
			document.getElementById("error").innerHTML = ""
		})
		.catch(function(e) {
			document.getElementById("error").innerHTML = "请求信息时发生错误: " +  e + "<br />" + (new Date())
			console.log("请求发生错误", e)
		})
}, 1000);
</script>
</body>
</html>		
	`))
}

/*
.catch(function (e) {
			document.getElementById("error").innerHTML = (new Date()) + "<br />" + 请求信息时发生错误: " +  e
			console.log("请求发生错误", e)
		})

 {
			document.getElementById("status").innerText = d
			document.getElementById("error").innerHTML = ""
		}
*/

/// 设置要使用的负载均衡器
/// 通过 POST balancer = [ Weighted | WeightedSquare | BestThroughput ] 进行设定
func webSetBalancer(w http.ResponseWriter, r *http.Request) {
	if webHandlePost(w, r) == false {
		return
	}

	balancer := r.FormValue("balancer")
	log.Printf("客户端要求将 balancer 设置为 `%s'", balancer)

	err := router.SetBalancer(balancer)
	if err != nil {
		fmt.Fprintf(w, "设置 balancer 失败: %v", err)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

/// 设置负载均衡器的判别标准
/// 通过 POST meter = [ SpeedBpsPerChannel1s | SpeedBpsPerChannel5s | SpeedBpsPerChannel15s | SpeedBpsPerChannel1m ] 进行设定
func webSetMeter(w http.ResponseWriter, r *http.Request) {
	if webHandlePost(w, r) == false {
		return
	}

	meter := r.FormValue("meter")
	log.Printf("客户端要求将 meter 设置为 `%s'", meter)

	err := router.SetMeter(meter)
	if err != nil {
		fmt.Fprintf(w, "设置 meter 失败: %v", err)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func webSetForceDispatchNodeDuration(w http.ResponseWriter, r *http.Request) {
	if webHandlePost(w, r) == false {
		return
	}

	duration_str := r.FormValue("duration")
	duration, err := strconv.Atoi(duration_str)
	if err != nil {
		fmt.Fprintf(w, "无法解析输入的内容`%s': %v", duration_str, err)
		return
	}

	log.Printf("客户端要求将 ForceDispatchNodeDuration 设置成 %d 秒", duration)

	err = router.SetForceDispatchNodeDuration(time.Duration(duration) * time.Second)
	if err != nil {
		fmt.Fprintf(w, "设置 meter 失败: %v", err)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

/// 检查客户端请求是否是 POST 方法，同时自动调用 ParseForm 方法
/// 如果出错，自动向客户端输出错误信息。如果成功，返回 true，如果失败返回 false
func webHandlePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		fmt.Fprintf(w, "Invalid method")
		return false
	}

	err := r.ParseForm()
	if err != nil {
		fmt.Fprintf(w, "ParseForm(): %v", err)
		return false
	}

	return true
}
