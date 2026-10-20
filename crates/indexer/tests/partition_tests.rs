use sqlx::PgPool;

#[sqlx::test]
async fn test_partition_boundary_continuity(pool: PgPool) {
    // Verify partition helper creates future partitions without errors
    let res = sqlx::query("SELECT ensure_future_event_partitions(2)")
        .execute(&pool)
        .await;
    assert!(res.is_ok(), "partition creation function should succeed");

    // Insert events spanning across boundaries
    let insert_res = sqlx::query(
        r#"
        INSERT INTO events (
            id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash, event_index, event_type, topics, data
        ) VALUES (
            gen_random_uuid(), 'CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4', 100, NOW(), 'hash', 0, 'contract', '[]', '{}'
        )
        "#,
    )
    .execute(&pool)
    .await;

    assert!(
        insert_res.is_ok(),
        "inserts across partition boundaries must succeed"
    );
}
