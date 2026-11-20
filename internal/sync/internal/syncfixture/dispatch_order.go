package syncfixture

// DispatchOrderEnv runs every engine the package's tests build under one
// order: "forward", "reverse" or "shuffle:<seed>". Unset, engines keep the
// graph order and the worker pool, as in production.
const DispatchOrderEnv = "ONEDRIVE_GO_DISPATCH_ORDER"
