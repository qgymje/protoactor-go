package actor

import (
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/emirpasic/gods/stacks/linkedliststack"
)

type messageSender struct {
	Message interface{}
	Sender  *PID
}

type localContext struct {
	message        interface{}
	parent         *PID
	self           *PID
	actor          Actor
	supervisor     SupervisorStrategy
	producer       Producer
	middleware     ReceiveFunc
	behavior       behaviorStack
	receive        ReceiveFunc
	children       PIDSet
	watchers       PIDSet
	watching       PIDSet
	stash          *linkedliststack.Stack
	stopping       bool
	restarting     bool
	receiveTimeout time.Duration
	t              *time.Timer
	restartStats   *ChildRestartStats
}

func newLocalContext(producer Producer, supervisor SupervisorStrategy, middleware ReceiveFunc, parent *PID) *localContext {
	cell := &localContext{
		parent:     parent,
		producer:   producer,
		supervisor: supervisor,
		middleware: middleware,
	}
	cell.incarnateActor()
	return cell
}

func (ctx *localContext) Actor() Actor {
	return ctx.actor
}

func (ctx *localContext) Message() interface{} {
	userMessage, ok := ctx.message.(*messageSender)
	if ok {
		return userMessage.Message
	}
	return ctx.message
}

func (ctx *localContext) Sender() *PID {
	userMessage, ok := ctx.message.(*messageSender)
	if ok {
		return userMessage.Sender
	}
	return nil
}

func (ctx *localContext) Stash() {
	if ctx.stash == nil {
		ctx.stash = linkedliststack.New()
	}

	ctx.stash.Push(ctx.message)
}

func (ctx *localContext) cancelTimer() {
	if ctx.t != nil {
		ctx.t.Stop()
		ctx.t = nil
	}
}

func (ctx *localContext) receiveTimeoutHandler() {
	ctx.self.Request(receiveTimeoutMessage, nil)
}

func (ctx *localContext) SetReceiveTimeout(d time.Duration) {
	if d == ctx.receiveTimeout {
		return
	}
	if ctx.t != nil {
		ctx.t.Stop()
	}

	if d < time.Millisecond {
		// anything less than than 1 millisecond is set to zero
		d = 0
	}

	ctx.receiveTimeout = d
	if d > 0 {
		if ctx.t == nil {
			ctx.t = time.AfterFunc(d, ctx.receiveTimeoutHandler)
		} else {
			ctx.t.Reset(d)
		}
	}
}

func (ctx *localContext) ReceiveTimeout() time.Duration {
	return ctx.receiveTimeout
}

func (ctx *localContext) Children() []*PID {
	r := make([]*PID, ctx.children.Len())
	ctx.children.ForEach(func(i int, p PID) {
		r[i] = &p
	})
	return r
}

func (ctx *localContext) Self() *PID {
	return ctx.self
}

func (ctx *localContext) Parent() *PID {
	return ctx.parent
}

func (ctx *localContext) Receive(message interface{}) {
	ctx.processMessage(message)
}

func (ctx *localContext) EscalateFailure(who *PID, reason interface{}, message interface{}) {

	//TODO: maybe we don't need the "who" here, as it is always the escalating actor that needs to pass itself
	//e.g. if actor c escalates to b that escalates to a, then a should try to handle b and not c

	//lazy initialize the child restart stats if this is the first time
	//further mutations are handled within "restart"
	if ctx.restartStats == nil {
		ctx.restartStats = &ChildRestartStats{
			FailureCount: 1,
		}
	}
	failure := &Failure{Reason: reason, Who: ctx.self, ChildStats: ctx.restartStats}
	if ctx.parent == nil {
		handleRootFailure(failure)
	} else {
		//TODO: Akka recursively suspends all children also on failure
		//Not sure if I think this is the right way to go, why do children need to wait for their parents failed state to recover?
		ctx.self.sendSystemMessage(suspendMailboxMessage)
		ctx.parent.sendSystemMessage(failure)
	}
}

func (ctx *localContext) InvokeUserMessage(md interface{}) {
	influenceTimeout := true
	if ctx.receiveTimeout > 0 {
		_, influenceTimeout = md.(NotInfluenceReceiveTimeout)
		influenceTimeout = !influenceTimeout
		if influenceTimeout {
			ctx.t.Stop()
		}
	}

	ctx.processMessage(md)

	if ctx.receiveTimeout > 0 && influenceTimeout {
		ctx.t.Reset(ctx.receiveTimeout)
	}
}

// localContextReceiver is used when middleware chain is required
func localContextReceiver(ctx Context) {
	a := ctx.(*localContext)
	if _, ok := a.message.(*PoisonPill); ok {
		a.self.Stop()
	} else {
		a.receive(ctx)
	}
}

func (ctx *localContext) processMessage(m interface{}) {
	ctx.message = m

	if ctx.middleware != nil {
		ctx.middleware(ctx)
	} else {
		if _, ok := m.(*PoisonPill); ok {
			ctx.self.Stop()
		} else {
			ctx.receive(ctx)
		}
	}
}

func (ctx *localContext) incarnateActor() {
	actor := ctx.producer()
	ctx.restarting = false
	ctx.stopping = false
	ctx.actor = actor
	ctx.receive = actor.Receive
}

func (ctx *localContext) InvokeSystemMessage(message SystemMessage) {
	switch msg := message.(interface{}).(type) {
	case *Started:
		ctx.InvokeUserMessage(msg) // forward
	case *Watch:
		ctx.watchers.Add(msg.Watcher)
	case *Unwatch:
		ctx.watchers.Remove(msg.Watcher)
	case *SuspendMailbox, *ResumeMailbox:
	//pass
	case *Stop:
		ctx.handleStop(msg)
	case *Terminated:
		ctx.handleTerminated(msg)
	case *Failure:
		ctx.handleFailure(msg)
	case *Restart:
		ctx.handleRestart(msg)
	default:
		log.Printf("Unknown system message %T", msg)
	}
}

func (ctx *localContext) handleRestart(msg *Restart) {
	ctx.stopping = false
	ctx.restarting = true
	ctx.InvokeUserMessage(restartingMessage)
	ctx.children.ForEach(func(_ int, pid PID) {
		pid.Stop()
	})
	ctx.tryRestartOrTerminate()
}

//I am stopping
func (ctx *localContext) handleStop(msg *Stop) {
	ctx.stopping = true
	ctx.restarting = false

	ctx.InvokeUserMessage(stoppingMessage)
	ctx.children.ForEach(func(_ int, pid PID) {
		pid.Stop()
	})
	ctx.tryRestartOrTerminate()
}

//child stopped, check if we can stop or restart (if needed)
func (ctx *localContext) handleTerminated(msg *Terminated) {
	ctx.children.Remove(msg.Who)
	ctx.watching.Remove(msg.Who)

	ctx.InvokeUserMessage(msg)
	ctx.tryRestartOrTerminate()
}

//offload the supervision completely to the supervisor strategy
func (ctx *localContext) handleFailure(msg *Failure) {
	if strategy, ok := ctx.actor.(SupervisorStrategy); ok {
		strategy.HandleFailure(ctx, msg.Who, msg.ChildStats, msg.Reason, msg.Message)
		return
	}
	ctx.supervisor.HandleFailure(ctx, msg.Who, msg.ChildStats, msg.Reason, msg.Message)
}

func (ctx *localContext) tryRestartOrTerminate() {
	if ctx.t != nil {
		ctx.t.Stop()
		ctx.t = nil
		ctx.receiveTimeout = 0
	}

	if !ctx.children.Empty() {
		return
	}

	if ctx.restarting {
		ctx.restart()
		return
	}

	if ctx.stopping {
		ctx.stopped()
	}
}

func (ctx *localContext) restart() {
	ctx.incarnateActor()
	//create a new childRestartStats with the current failure settings
	ctx.restartStats = &ChildRestartStats{
		FailureCount:    ctx.restartStats.FailureCount + 1,
		LastFailureTime: time.Now(),
	}
	ctx.InvokeUserMessage(startedMessage)
	if ctx.stash != nil {
		for !ctx.stash.Empty() {
			msg, _ := ctx.stash.Pop()
			ctx.InvokeUserMessage(msg)
		}
	}
	ctx.self.sendSystemMessage(resumeMailboxMessage)
}

func (ctx *localContext) stopped() {
	ProcessRegistry.Remove(ctx.self)
	ctx.InvokeUserMessage(stoppedMessage)
	otherStopped := &Terminated{Who: ctx.self}
	ctx.watchers.ForEach(func(i int, pid PID) {
		pid.sendSystemMessage(otherStopped)
	})
}

func (ctx *localContext) SetBehavior(behavior ReceiveFunc) {
	ctx.behavior.Clear()
	ctx.receive = behavior
}

func (ctx *localContext) PushBehavior(behavior ReceiveFunc) {
	ctx.behavior.Push(ctx.receive)
	ctx.receive = behavior
}

func (ctx *localContext) PopBehavior() {
	if ctx.behavior.Len() == 0 {
		panic("Cannot unbecome actor base behavior")
	}
	ctx.receive, _ = ctx.behavior.Pop()
}

func (ctx *localContext) Watch(who *PID) {
	who.sendSystemMessage(&Watch{
		Watcher: ctx.self,
	})
	ctx.watching.Add(who)
}

func (ctx *localContext) Unwatch(who *PID) {
	who.sendSystemMessage(&Unwatch{
		Watcher: ctx.self,
	})
	ctx.watching.Remove(who)
}

func (ctx *localContext) Respond(response interface{}) {
	if ctx.Sender() == nil {
		log.Fatal("[ACTOR] No sender")
	}
	ctx.Sender().Tell(response)
}

func (ctx *localContext) Spawn(props Props) *PID {
	pid, _ := ctx.SpawnNamed(props, ProcessRegistry.NextId())
	return pid
}

func (ctx *localContext) SpawnNamed(props Props, name string) (*PID, error) {
	pid, err := props.spawn(ctx.self.Id+"/"+name, ctx.self)
	if err != nil {
		return pid, err
	}

	ctx.children.Add(pid)
	ctx.Watch(pid)

	return pid, nil
}

func (ctx *localContext) GoString() string {
	return fmt.Sprintf("%v/%v:%v", ctx.self.Address, ctx.self.Id, reflect.TypeOf(ctx.actor))
}

func handleRootFailure(msg *Failure) {
	defaultSupervisionStrategy.HandleFailure(nil, msg.Who, msg.ChildStats, msg.Reason, msg.Message)
}
