# Rust job service protocol

The worked service-path example — submit, keep the receipt, observe, reconcile,
cancel — is in [../README.md](../README.md#an-example-that-runs); this crate is
the generated wire vocabulary the validated binding below sits on.

```rust
use abstraction_facade_jobs::{jobs::wire, JobsMachine};
use abstraction_facade_native::Machine;
use abstraction_facade_service::wire::Scope;

fn run(endpoint: &str) -> Result<(), String> {
    let jobs = Machine::new(endpoint)
        .resolve_jobs(vec![], Scope::Local)
        .map_err(|e| format!("{e:?}"))?;
    let window = jobs.history_window().map_err(|e| format!("{e:?}"))?;
    let identity = wire::RequestIdentity { key: "example-work".into(), history_epoch: window.history_epoch, attempt: 0 };
    let submission = wire::Submission {
        identity: identity.clone(), kind: "example".into(),
        spec: br#"{"fetch":"anything at all"}"#.to_vec(),
        required_guarantees: vec![], label: String::new(),
    };
    let accepted = jobs.submit(submission).map_err(|e| format!("{e:?}"))?;
    if accepted.outcome != "accepted" {
        return Err(format!("submission not accepted: {}", accepted.outcome));
    }
    let observed = jobs.observe(&identity).map_err(|e| format!("{e:?}"))?;
    println!("state: {}", observed.outcome);
    jobs.cancel_work(&identity).map_err(|e| format!("{e:?}"))?;
    Ok(())
}
```

`abstraction-job-api` exports generated RecoverableAcceptance, OperationControl and JobInventory vocabulary/clients. Generate from acceptance.thrift with the repository generate.targets recipe. Binary payloads use Vec<u8> and canonical base64. The crate has no native platform I/O or external Cargo dependencies.

`jobs` above is that validated, resolved binding: `Machine::resolve_jobs`, through the `JobsMachine` trait, returns `abstraction_facade_jobs::jobs::Jobs`, and its `submit`, `observe`, `reconcile`, `cancel_work`, `read_result` and `copy_result` methods are what the example calls. These source Cargo packages have development version 0.0.0; registry publication is not claimed.
