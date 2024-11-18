# GoTLCP HTTPS 配置

## 1. HTTPS原理概述

TLCP协议作为传输层密码协议，在握手完成后认为建立TLCP连接，对于上层应用来说TLCP是透明的，上层应用依然可以使用Socket接口进行通信，
下层通信实现为TLCP协议，而不是TCP协议。

基于上述原理可以将HTTP下方的协议替换为TLCP协议，就这就是HTTPS，协议栈结构如下所示：

![HTTPS](./img/HTTPS.png)


## 2. 服务端

> 若您需要配置双向身份认证请参考 [《GoTLCP 服务端配置》](./ServerConfig.md)

### 2.1 Go标准库 HTTPS

标准库实现TLCP HTTPS流程如下：

1. 创建 `http.Server` 对象，设置HTTP路由等。
2. 通过 `tlcp.Listen` 方法，配置并启动TLCP Listener。
3. `http.Server` 对象的`Serve` 方法传入TLCP Listener对象。

```go
package main

import (
	"github.com/geekxcan/gotlcp/tlcp"
	"net/http"
)

func main() {
	// 省略部分初始化代码...
	
	config := &tlcp.Config{Certificates: certKeys}

	serveMux := http.NewServeMux()
	// 设置HTTP路由
	serveMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Hello GoTLCP!"))
	})
	// 1. 构造 HTTP服务
	svr := http.Server{Addr: ":443", Handler: serveMux}
	// 2. 创建 TLCP Listen
	ln, err := tlcp.Listen("tcp", svr.Addr, config)
	if err != nil {
		panic(err)
	}
	// 3. 通过 TLCP Listen 启动HTTPS服务
	err = svr.Serve(ln)
	if err != nil {
		panic(err)
	}
}
```

完整示例见 [https/server/std/main.go](../example/https/server/std/main.go)

## 2.2 Gin 配置

> [Gin Web Framework](https://github.com/gin-gonic/gin)

与标准Go的HTTPS配置类似的，Gin的HTTPS配置方式如下：

1. 创建 `gin.Engine` 对象，作为HTTP服务的处理函数。
2. 创建 `http.Server` 对象，设置HTTP路由等。
3. 通过 `tlcp.Listen` 方法，配置并启动TLCP Listener。
4. `http.Server` 对象的`Serve` 方法传入TLCP Listener对象。


```go
package main

import (
	"github.com/geekxcan/gotlcp/tlcp"
	"github.com/gin-gonic/gin"
	"net/http"
)

func main() {
	config := &tlcp.Config{Certificates: load()}

	// 1. 创建 gin 的HTTP处理器
	router := gin.Default()
	router.GET("/", func(ctx *gin.Context) { ctx.String(200, "Hello GoTLCP Gin!") })

	// 2. 通过Gin的处理器构造 HTTP服务
	svr := http.Server{Addr: ":443", Handler: router}
	// 3. 创建 TLCP Listen
	ln, err := tlcp.Listen("tcp", svr.Addr, config)
	if err != nil {
		panic(err)
	}
	// 4. 通过 TLCP Listen 启动HTTPS服务
	err = svr.Serve(ln)
	if err != nil {
		panic(err)
	}
}
```

完整示例见 [https/server/gin_demo/main.go](../example/https/server/gin_demo/main.go)

### 2.3 Fiber

> [Fiber](https://github.com/gofiber/fiber)

与标准Go的HTTPS配置类似的，Fiber的HTTPS配置方式如下：

1. 创建 `fiber.App` 对象，注册路由。
2. 通过 `tlcp.Listen` 方法，配置并启动TLCP Listener。
3. `fiber.App` 对象的`Listener` 方法传入TLCP Listener对象。

```go
package main

import (
	"github.com/geekxcan/gotlcp/tlcp"
	"github.com/gofiber/fiber/v2"
)

func main() {
	config := &tlcp.Config{Certificates: load()}
	// 1. 创建Fiber应用， 注册路由
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello GoTLCP Fiber")
	})
	// 2. 创建 TLCP Listen
	ln, err := tlcp.Listen("tcp", ":3000", config)
	if err != nil {
		panic(err)
	}
	err = app.Listener(ln)
	if err != nil {
		panic(err)
	}
}
```

完整示例见 [https/server/fiber_demo/main.go](../example/https/server/fiber_demo/main.go)

## 3. 客户端

GoTLCP提供了简单的方法来构造HTTPS客户端，构造的HTTP客户端您可以和普通的HTTP客户端一样使用没有差别。

配置如下：

1. 初始化TLCP配置。
2. 创建TLCP HTTPS客户端。
3. 使用 HTTP客户端通信。

```go
package main

import (
	"gitee.com/Trisia/gotlcp/https"
	"github.com/geekxcan/gotlcp/tlcp"
	"io"
	"os"
)

func main() {
	// 1. 初始化TLCP配置
	config := &tlcp.Config{RootCAs: load()}

	// 2. 创建TLCP HTTPS客户端
	client := https.NewHTTPSClient(config)
	
	// 3. 使用 HTTP客户端通信
	resp, err := client.Get("https://127.0.0.1")
	if err != nil {
		panic(err)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil && err != io.EOF {
		panic(err)
	}
}

```

若您需要客户端连接超时时间进行配置，请使用：

- `https.NewHTTPSClientDialer(dialer *net.Dialer, config *tlcp.Config) *http.Client`


> 若您需要配置双向身份认证请参考 [《GoTLCP 客户端配置》](./ClientConfig.md)

完整示例见 [https/client/main.go](../example/https/client/main.go)



## 4. HTTPS TLCP/TLS自适应服务端

> 关于 TLCP/TLS 自适应原理，请参考[《GoTLCP 协议适配器》](../pa/README.md)相关内容。

以Go语言标准库为例，TLCP/TLS 自适应HTTPS服务端配置方式如下：

1. 创建PA Listener。
2. 构造HTTP服务。
3. 通过 PA Listen 启动HTTPS服务。

```go
package main

import (
	"crypto/tls"
	"gitee.com/Trisia/gotlcp/pa"
	"github.com/geekxcan/gotlcp/tlcp"
	"net/http"
)

var (
	sigCert tlcp.Certificate
	encCert tlcp.Certificate

	rsaCert tls.Certificate
)

func main() {
	var err error
	tlcpCfg := &tlcp.Config{
		Certificates: []tlcp.Certificate{sigCert, encCert},
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{rsaCert},
	}
	// 1. 创建PA Listener
	ln, err := pa.Listen("tcp", ":443", tlcpCfg, tlsCfg)
	if err != nil {
		panic(err)
	}
	// 2. 构造HTTP服务
	serveMux := http.NewServeMux()
	serveMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Hello GoTLCP!"))
	})
	svr := http.Server{Addr: ":443", Handler: serveMux}
	// 3. 通过 PA Listen 启动HTTPS服务
	err = svr.Serve(ln)
	if err != nil {
		panic(err)
	}
}
```

完整示例见 [https/server/pa/main.go](../example/https/server/pa/main.go)

类似的Gin 与 Fiber 的配置方式也是类似，详见示例：

- [Gin HTTPS 协议自适应 https/server/pa/gin_demo/main.go](../example/https/server/pa/gin_demo/main.go)
- [Fiber HTTPS 协议自适应 https/server/pa/fiber_demo/main.go](../example/https/server/pa/fiber_demo/main.go)


