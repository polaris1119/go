// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks.  Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines.  Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"sync/atomic"
	"unsafe"
)

// 互斥量

// A Mutex is a mutual exclusion lock.
// Mutexes can be created as part of other structures;
// the zero value for a Mutex is an unlocked mutex.
type Mutex struct {
	state int32  // 锁状态（锁变量）：0-未锁定；非0-锁定；它的作用是保护信号量
	sema  uint32 // 信号量：0-表示没有保存下来的唤醒操作（即没有 Unlock 操作）；正值—表示有一个或多个唤醒操作
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // mutex is locked
	mutexWoken
	mutexWaiterShift = iota
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// m.state 和 0(未锁定) 比较，如果相等，表示未锁定，然后将其锁定，并返回 true
	// 这个过程是原子的
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if raceenabled {
			raceAcquire(unsafe.Pointer(m))
		}
		return
	}

	// 执行到这里，表示已经被其他 goroutine 锁定了，需要阻塞

	awoke := false
	iter := 0 // 用户控制自旋锁重试次数（active_spin == 4）
	for {
		old := m.state
		new := old | mutexLocked
		if old&mutexLocked != 0 {
			if runtime_canSpin(iter) {
				// Active spinning makes sense.
				// Try to set mutexWoken flag to inform Unlock
				// to not wake other blocked goroutines.
				if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
					atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
					awoke = true
				}
				runtime_doSpin()
				iter++
				continue
			}
			new = old + 1<<mutexWaiterShift
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				panic("sync: inconsistent mutex state")
			}
			// 只保留一位: new = new & 1
			new &^= mutexWoken
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&mutexLocked == 0 {
				break
			}
			// 信号量的 down 操作：检查 m.sema 是否大于 0，若大于 0，则 m.sema--（即用掉保存的唤醒操作）并继续；
			// 若为 0，则该 goroutine 休眠，而且此时 runtime_Semacquire 并未结束
			runtime_Semacquire(&m.sema)
			awoke = true
			iter = 0
		}
	}

	if raceenabled {
		raceAcquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if raceenabled {
		_ = m.state
		raceRelease(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if (new+mutexLocked)&mutexLocked == 0 {
		panic("sync: unlock of unlocked mutex")
	}

	old := new
	for {
		// If there are no waiters or a goroutine has already
		// been woken or grabbed the lock, no need to wake anyone.
		if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken) != 0 {
			return
		}
		// Grab the right to wake someone.
		new = (old - 1<<mutexWaiterShift) | mutexWoken
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 信号量的 up 操作：m.sema++，并选择一个等待的 goroutine ，将其唤醒
			runtime_Semrelease(&m.sema)
			return
		}
		old = m.state
	}
}
