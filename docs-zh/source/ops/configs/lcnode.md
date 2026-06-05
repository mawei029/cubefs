# LcNode 配置
## 配置说明

| 参数           | 类型           | 描述                                                              | 必需  | 默认值     |
|:--------------|:--------------|:-----------------------------------------------------------------|:-------| :--------- |
| role         | string       | 进程角色，必须设置为 `lcnode`                                         | 是   |          |
| listen       | string       | http 服务监听的端口号. 格式: `PORT`         | 是   |     80     |
| logDir       | string       | 日志存放路径                                                          | 是   |          |
| logLevel     | string       | 日志级别                                              | 否   |    error      |
| masterAddr   | string slice | 格式: `HOST:PORT`，HOST: 资源管理节点IP（Master），PORT: 资源管理节点服务端口（Master） | 是   |          |
| prof         | string       | 调试和管理员 API 接口                                                     | 否   |          |
| lcScanRoutineNumPerTask | int       | 生命周期迁移文件并发数                                                     | 否   |     20     |
| lcScanLimitPerSecond    | int       | 生命周期迁移文件QPS限制                                                     | 否   |    0 （不限制）      |
| lcReadBandwidthLimitMB  | int       | 单 lcnode 生命周期迁移读带宽限制，单位 MB/s，0 表示不限制                      | 否   |    0 （不限制）      |
| lcWriteBandwidthLimitMB | int       | 单 lcnode 生命周期迁移写带宽限制，单位 MB/s，0 表示不限制                      | 否   |    0 （不限制）      |
| delayDelMinute | int       | 生命周期迁移文件后源数据保留时间                                                     | 否   |     1440     |
| useCreateTime | bool       | 生命周期使用文件创建时间判断过期，默认使用文件访问时间判断过期                    | 否   |     false     |

## 配置示例

``` json
{
    "role": "lcnode",
    "listen": "17510",
    "logDir": "./logs",
    "logLevel": "info",
    "masterAddr": [
        "xxx",
        "xxx",
        "xxx"
    ],
    "lcReadBandwidthLimitMB": 100,
    "lcWriteBandwidthLimitMB": 50
}
```

## 运行时调整读写带宽

lcnode 的 HTTP 管理端口为 `listen + 1`，可通过以下接口调整当前节点生命周期迁移读写带宽：

``` bash
curl -X POST "http://<lcnode>:<httpPort>/setLcIoLimit?readMBps=100&writeMBps=50"
curl "http://<lcnode>:<httpPort>/getLcIoLimit"
```

其中 `readMBps` 和 `writeMBps` 单位为 MB/s，设置为 `0` 表示不限速。该接口只调整当前 lcnode 进程内存中的限速值，不会持久化到配置文件。
`getLcIoLimit` 返回值仅展示字节单位：`readBytesPerSec` 与 `writeBytesPerSec`。

## 安全注意事项

lcnode HTTP 管理端口（`listen + 1`）提供了运维管理接口（包括停止扫描、获取文件、动态调整带宽限流等操作），**必须通过防火墙或安全组限制仅允许可信内网访问**，禁止将该端口直接暴露到公网或不可信网络。

### 读写配额计费说明

读写带宽分别计量，计费规则如下：

| 场景 | 读配额 | 写配额 |
|:-----|:------|:------|
| 池间迁移拷贝（读源池 + 写目标池） | 按源池读取字节数计 | 按目标池写入字节数计 |
| 迁移后 MD5 校验读 | 按读取字节数计 | 不计 |
| 迁 EBS（读 extent + Put） | 按 extent 读取字节数计 | 按 Put 写出字节数计 |
| 迁 EBS 后 MD5 校验读 | 按读取字节数计 | 不计 |
| HTTP `/getFile` 下载 | 按读取字节数计 | 不计 |

因此，池间迁移拷贝路径会同时受 `lcReadBandwidthLimitMB` 与 `lcWriteBandwidthLimitMB` 约束：源池读取速度不超过读上限，目标池写入速度不超过写上限。校验阶段会额外消耗读配额。