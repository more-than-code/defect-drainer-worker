package workerproto

// A generated Stack sign-in password (worker protocol v1 §17). Additive to
// v1.

// CapSigninRandomV1: the worker accepts JobSpec.project.jobStackSignIn
// .password set to exactly "{random}" and fills it with the run's {random}
// value. A server offers such a job only to a worker that lists it.
const CapSigninRandomV1 = "signin_random.v1"
