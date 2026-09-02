-- Integration tests create one database per test for isolation, because MySQL
-- has no schemas: a database IS the namespace.
--
-- The grant is scoped to the it_% prefix rather than given globally, so the test
-- user stays a non-superuser. A statement that needs more privilege than a real
-- deployment would grant fails here rather than in someone's production rollout.
GRANT ALL PRIVILEGES ON `it\_%`.* TO 'datagit'@'%';
GRANT CREATE, DROP ON *.* TO 'datagit'@'%';
FLUSH PRIVILEGES;
