# cfrouter
一个 TCP 节点转发程序，可以自动选择最快的节点转发 TCP 连接

## 安装
```
git clone github.com/greensea/cfrouter
cd cfrouter
go get
go build
```
也可以直接到 release 页面下载编译好的文件。

## 使用
1. 创建 cf_targets.txt 文件，将服务器列表填入文件中，每行一个服务器，必须包含端口号。可以查看 cf_targets.example.txt 查看范例
2. 运行 cfrouter
```
./cfrouter
```
3. 如果启动成功，可以打开 https://localhost:5531 查看统计和管理页面
4. 现在 cfrouter 已经监听在 127.0.0.1:5530 端口，并将连接到此端口的 TCP 连接转发到 cf_targets.txt 中指定的服务器了。

## 特性
* 自动选择远程服务器
* 自定义选择策略
* 自动检测死掉的远程服务器并禁用
* 自动检测死掉的远程服务器是否复活了并激活
* 精确到每个远程服务器的统计信息，可在管理页面查看

## 相对 haproxy 有什么优势？
haproxy 不支持根据服务器速度进行负载均衡，而 cfrouter 可以做到这一点。cfrouter 可以优先将连接分配到传输速度最快的服务器上。这也是作者开发这个工具的初衷。

## 其他
如果你觉得 cfrouter 很有用，或者发掘了新的功能，欢迎提交使用案例，我会在这里列出来。

## 管理页面预览
![](https://raw.githubusercontent.com/greensea/cfrouter/master/manage_preview.png)

