package workerproto

// Job messages (worker protocol v1 §20): people talk with a running job's
// coding agent, and a finished batch fix answers them in a read-only chat
// run. Additive to v1.
//
//   - Controls "message" and "interrupt" carry control.messages to a
//     running job; "interrupt" also stops the agent mid-step to take them.
//     Both answer {} once the worker holds the messages, and fail with
//     code internal and detail DetailJobNotRunning when no run is live.
//   - JobSpec.messaging turns the inbox and outbox on; JobSpec.mode "chat"
//     with JobSpec.chat is a chat run, started by a resume control.
//   - Events "message", "delivered", "waiting" and "chat" report them.
//     A message event's images travel as blobs, like harvested images.

// CapJobMessagesV1: the worker understands §20.
const CapJobMessagesV1 = "job_messages.v1"

// DetailJobNotRunning is a message control's failure detail when the job
// has no live run.
const DetailJobNotRunning = "job is not running"

// MaxControlMessages bounds control.messages.
const MaxControlMessages = 10
