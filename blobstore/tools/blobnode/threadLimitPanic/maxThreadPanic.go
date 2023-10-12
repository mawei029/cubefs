package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"
)

// go build -o thPanic maxThreadPanic.go

// #!/bin/bash
//echo "$(date)"
//./thPanic &
//pid=`ps aux | grep thPanic | grep -v grep | awk '{print $2}'`
//top -Hp $pid

func main() {
	fmt.Printf("start... num go:%d, maxprocs:%d, numcpu:%d \n",
		runtime.NumGoroutine(), runtime.GOMAXPROCS(-1), runtime.NumCPU())
	printLog()
	//num := 1

	//test1()
	//test2(&num)
	test3()
}

func printLog() {
	go func() {
		tk := time.NewTicker(time.Second)
		threadProfile := pprof.Lookup("threadcreate")

		for {
			//tk.Reset(time.Second*2)
			select {
			case <-tk.C:
			}
			fmt.Printf("num go:%d, num th:%d \n", runtime.NumGoroutine(), threadProfile.Count())
		}
	}()
}

// sleep or block
func test1() {
	fmt.Println("start...")

	cnt := int64(0)

	fmt.Printf("num go:%d, cnt:%d, maxprocs:%d, numcpu:%d \n",
		runtime.NumGoroutine(), atomic.LoadInt64(&cnt), runtime.GOMAXPROCS(-1), runtime.NumCPU()) // 2,0,6,6
	//time.Sleep(time.Second * 1)
	//os.Exit(0)

	for i := 0; i < 32; i++ {
		go func() {
			for {
				atomic.AddInt64(&cnt, 2)
				time.Sleep(time.Millisecond)
				atomic.AddInt64(&cnt, -1)
			}
		}()
	}

	ch := make(chan int64)
	for i := 0; i < 100000; i++ {
		go func() {
			for {
				atomic.AddInt64(&cnt, 2)
				ch <- cnt
				//time.Sleep(time.Microsecond)
				//atomic.AddInt64(&cnt, -1)
			}
		}()
	}

	time.Sleep(time.Second * 16)
	fmt.Println("end...")
}

// sleep
func test2(cnt *int) {
	var wg sync.WaitGroup

	for i := 0; i < 2000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			*cnt = i
			// fmt.Printf("Goroutine %d started\n", i)
			time.Sleep(10 * time.Second)
			// fmt.Printf("Goroutine %d ended\n", i)
			*cnt = i + 2
		}(i)
	}

	wg.Wait()
	fmt.Println("All goroutines finished executing")
}

func doBash(i int, wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
		if err := recover(); err != nil {
			fmt.Println("ERROR...")
		}
		//recover()
	}()

	time.Sleep(time.Second * time.Duration(i))
	_, err := exec.Command("bash", "-c", "sleep 10").Output()
	if err != nil {
		//time.Sleep(time.Second * 2)
		fmt.Printf("num go:%d, maxprocs:%d, numcpu:%d \n",
			runtime.NumGoroutine(), runtime.GOMAXPROCS(-1), runtime.NumCPU())
		panic(err)
		runtime.Goexit()
	}
}

// it simulate system call(block)
func test3() {
	debug.SetMaxThreads(16)

	//time.Sleep(time.Second * 2)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go doBash(i, &wg)
	}

	//time.Sleep(time.Second * 12)
	wg.Wait()
	fmt.Printf("panic... num go:%d, maxprocs:%d, numcpu:%d \n",
		runtime.NumGoroutine(), runtime.GOMAXPROCS(-1), runtime.NumCPU())
}
