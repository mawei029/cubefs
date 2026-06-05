# LcNode Configuration
## Configuration Description

| Parameter           | Type           | Description                                                              | Required  | Default Value     |
|:--------------|:--------------|:-----------------------------------------------------------------|:-------| :--------- |
| role         | string       | Process role, must be set to `lcnode`                                         | Yes   |       |
| listen       | string       | Port number for HTTP service listening. Format: `PORT`                   | Yes      |   80    |
| logDir       | string       | Path to store logs                                                          | Yes   |       |
| logLevel     | string       | Log level                                                                   | No   |   error    |
| masterAddr   | string slice | Format: `HOST:PORT`, HOST: Resource management node IP (Master), PORT: Resource management node service port (Master) | Yes   |       |
| prof         | string       | Debugging and administrator API interface                                   | No   |       |
| lcScanRoutineNumPerTask | int       | Number of concurrent file migration                                 | No   |     20     |
| lcScanLimitPerSecond    | int       | QPS limit of file migration                                         | No   |    0 (no limit)      |
| lcReadBandwidthLimitMB  | int       | Per-lcnode read bandwidth limit for lifecycle migration, in MB/s; 0 means no limit | No   |    0 (no limit)      |
| lcWriteBandwidthLimitMB | int       | Per-lcnode write bandwidth limit for lifecycle migration, in MB/s; 0 means no limit | No   |    0 (no limit)      |
| delayDelMinute | int       | Lifecycle Source data retention period after a file is migrated               | No   |     1440     |
| useCreateTime | bool       | Use file create time to determine expiration. By default, use file access time to determine expiration  | No   |     false     |

## Configuration Example

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

## Runtime Read/Write Bandwidth Adjustment

The lcnode HTTP admin port is `listen + 1`. You can adjust the current node's lifecycle migration read/write bandwidth limits via:

``` bash
curl -X POST "http://<lcnode>:<httpPort>/setLcIoLimit?readMBps=100&writeMBps=50"
curl "http://<lcnode>:<httpPort>/getLcIoLimit"
```

`readMBps` and `writeMBps` are in MB/s. Set them to `0` to disable throttling. These APIs only update the in-memory limits of the current lcnode process and do not persist changes to the configuration file.
`getLcIoLimit` response only exposes byte-based fields: `readBytesPerSec` and `writeBytesPerSec`.

## Security Notes

The lcnode HTTP admin port (`listen + 1`) provides several operational management interfaces including stopping scanners, downloading files, and dynamically adjusting bandwidth limits. **This port must be restricted to trusted internal networks only via firewall or security groups**, and must not be directly exposed to the public Internet or untrusted networks.

### Read/write quota accounting

Read and write bandwidth are accounted separately:

| Scenario | Read quota | Write quota |
|:---------|:-----------|:------------|
| Pool-to-pool copy (read source + write destination) | Charged per source-read byte | Charged per destination-write byte |
| Post-migration MD5 verification read | Charged per byte read | Not charged |
| Migrate to EBS (read extent + Put) | Charged per extent-read byte | Charged per Put byte |
| Post-EBS-migration MD5 verification read | Charged per byte read | Not charged |
| HTTP `/getFile` download | Charged per byte read | Not charged |

Pool-to-pool copy is constrained by both `lcReadBandwidthLimitMB` and `lcWriteBandwidthLimitMB`: source reads must stay within the read limit, and destination writes must stay within the write limit. Verification reads consume additional read quota.