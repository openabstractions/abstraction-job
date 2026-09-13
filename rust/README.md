# Rust job service protocol

`abstraction-job-api` exports generated RecoverableAcceptance, OperationControl and JobInventory vocabulary/clients. Generate from acceptance.thrift with the repository generate.targets recipe. Binary payloads use Vec<u8> and canonical base64. The crate has no native platform I/O or external Cargo dependencies.

Applications can use abstraction-facade-jobs::jobs for validated, resolved bindings. These source Cargo packages have development version0.0.0; registry publication is not claimed.
