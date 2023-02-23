package main

import (
	"fmt"
	"sync"
)

type Node struct {
	data interface{}
	next *Node
}

type MyQueue struct {
	head *Node //栈顶指针
	tail *Node //栈顶指针
	size int
	lock sync.RWMutex
}

//创建
func newListQueue() *MyQueue {
	return &MyQueue{}
}

//判空
func (q *MyQueue) isEmpty() bool {
	return q.size == 0
}

//获取队列大小
func (q *MyQueue) getSize() int {
	return q.size
}

//获取队头数据
func (q *MyQueue) getFront() interface{} {
	if q.isEmpty() {
		fmt.Println("空队列！")
		return nil
	}
	return q.head.data
}

//入队 链表尾插法
func (q *MyQueue) push(val interface{}) {
	q.lock.Lock()
	defer q.lock.Unlock()

	node := Node{data: val}
	if q.isEmpty() {
		q.head = &node //队头
		q.tail = &node //队尾
		q.size++
		return
	}

	q.tail.next = &node // 插入尾部
	q.tail = &node      // 指针后移
	q.size++
}

func (q *MyQueue) pushs(vals ...interface{}) {
	for _, v := range vals {
		q.push(v)
	}
}

//出队
func (q *MyQueue) pop() interface{} {
	q.lock.Lock()
	defer q.lock.Unlock()
	if q.isEmpty() {
		fmt.Println("空队列!")
		return nil
	}
	val := q.head.data
	q.head = q.head.next // 头部后移
	q.size--
	return val
}

//显示队列元素
func (q *MyQueue) showQueue() {
	if q.isEmpty() {
		fmt.Println("空队列！")
		return
	}
	tailTemp := q.head
	for i := 1; tailTemp != nil; i++ {
		fmt.Println("index:", i, "val:", tailTemp.data)
		tailTemp = tailTemp.next
	}
	return
}
