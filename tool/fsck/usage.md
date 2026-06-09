### Command examples

```example bash
./cfs-fsck check inode --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck check dentry --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck check both --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck check both --vol "<volName>" --inode-list "inodes.txt" --dentry-list "dens.txt"
./cfs-fsck check mp --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck check mp --master "127.0.0.1:17010" --vol "<volName>" --mport "17220" --mp 1
./cfs-fsck check mp --master "127.0.0.1:17010" --mport "17220" --mp 1
./cfs-fsck check mp --master "127.0.0.1:17010" --mport "17220" --mp 1 --check-apply-id true
./cfs-fsck clean evict --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck clean inode --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck clean inode --vol "<volName>" --inode-list "inodes.txt" --dentry-list "dens.txt"
./cfs-fsck clean dentry --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck clean dentry --vol "<volName>" --inode-list "inodes.txt" --dentry-list "dens.txt"
./cfs-fsck get locations --inode <inodeID> --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck get path --inode <inodeID> --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck get path --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck get summary --inode <inodeID> --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
./cfs-fsck check orphan-bloom --master "127.0.0.1:17010" --vol "<volName>" --mport "17220"
```

`check orphan-bloom` streams dentries into a bloom filter and reports inodes not referenced by any dentry.
It does not verify whether the dentry path is reachable from root inode `1`, so its result is not equivalent to
`check inode` root-reachability analysis. Results are written to `inode.dump.obsolete.bloom`, not
`inode.dump.obsolete`.

**WARNING**: Bloom filter has a theoretical false positive rate (FPR). False positives will cause actual orphan inodes to be missed from the result. This command accepts missing some cleanup candidates as the trade-off for lower memory usage and faster initial assessment. If the actual referenced inode count is higher than the estimated bloom capacity, the real FPR may also be higher. **Do not delete inodes based solely on this result**. Use it as a fast reference only.
