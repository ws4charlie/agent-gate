# 基础设施

GCP 部署以代码形式写在 `infra/terraform` 中,涵盖网络、SSH 防火墙规则、虚拟机、它的服务账号,以及它唯一可读的那个密钥。虚拟机内部运行的内容和密钥的内容不在其中。

这些代码是在部署存在之后才写的,所以它接管现有资源而不是新建。对真实项目执行 plan 应当显示没有变更;任何差异都是漂移,说明代码或项目有一边不对。

## 策略守护什么

`infra/policy` 中的策略用 OPA 的语言编写,每次变更都在 CI 中运行。目的是让任何人(包括 AI)都无法悄悄削弱现有设置:

- 只有当每条入站规则都只放行 Google IAP 地址段时,虚拟机才可以有公网地址;
- 不接受来自整个互联网的流量,SSH 只能经由 IAP 地址段;
- 虚拟机保持 Shielded VM;
- 不授予 owner 或 editor 角色;
- 密钥的值不进 Terraform;
- 启动脚本不携带应用配置。

每条规则都有一个通过和一个失败的测试。

## CI 不做什么

CI 从不连接 GCP,也不持有凭据,只检查格式、有效性和策略。plan 和 apply 在负责人的机器上运行,状态存放在人类一次性创建的存储桶中。

## 容器镜像和 Helm chart

仓库根目录的 `Dockerfile` 构建一个镜像,里面只有静态链接的 `agentgate` 二进制,基础镜像是 distroless。它以非 root 用户运行,没有 shell。迁移脚本、console 的网页文件和 mock USDC 合约都嵌在二进制里。构建上下文是白名单(`.dockerignore`),所以密钥、`data/` 和 `.env` 不会进入镜像。

```bash
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t agentgate:local .
```

`deploy/helm/agentgate` 中的 chart 用这个镜像跑两个程序:网关(`agentgate serve`),以及在 `paidtool.enabled` 时的 x402 付费工具(`agentgate paidtool`)。网关启动时执行迁移,提供 `/healthz`(进程在运行)和 `/readyz`(数据库可达)。日志为 JSON。

chart 只通过 values 配置,values 变成环境变量和挂载的文件。它从不创建 secret:数据库 URL、钱包密钥、QuorumVault 的证书和 client secret、Gate 凭证密钥、facilitator 密钥、模型提供方密钥,以及带 API key 的链 RPC URL,都来自你在 values 里写名字的 Secret。`signer: wallet` 或 `signer: vault` 选择签名方式;vault 模式下 chart 设置全部 `AG_VAULT_*` 并挂载文件,wallet 模式下一个都不设置。网关只接受自己拥有的私有文件作为 Gate 凭证密钥,所以先由一个 init 容器把它从 Secret 复制到内存目录。`networkPolicy.enabled` 拒绝所有流量,只放行 DNS、你列出的出站组(Postgres、Vault、令牌端点、链 RPC)和网关到付费工具。

在 kind 中试用:加载镜像,用你自己的 values 文件安装。`values-local.yaml` 是与 QuorumVault 同在本地集群时的示例。

```bash
kind load docker-image agentgate:local --name <cluster>
helm upgrade --install agentgate deploy/helm/agentgate -n agentgate --create-namespace -f <your values>
```

`scripts/check-chart.sh` 用两种签名方式检查和渲染 chart,并确认它设置的每个环境变量都列在 `.env.example` 中。CI 在 `helm` job 里运行它。
