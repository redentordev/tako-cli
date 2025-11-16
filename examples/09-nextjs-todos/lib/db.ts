import Database from "better-sqlite3";
import path from "path";
import fs from "fs";

// Database path - use environment variable or default to ./data directory
const DB_PATH = process.env.DATABASE_PATH || path.join(process.cwd(), "data", "todos.db");

// Ensure data directory exists
const dbDir = path.dirname(DB_PATH);
if (!fs.existsSync(dbDir)) {
  fs.mkdirSync(dbDir, { recursive: true });
}

// Create database connection
const db = new Database(DB_PATH);

// Enable foreign keys
db.pragma("foreign_keys = ON");

// Initialize database schema
function initializeDatabase() {
  db.exec(`
    CREATE TABLE IF NOT EXISTS todos (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      title TEXT NOT NULL,
      completed BOOLEAN NOT NULL DEFAULT 0,
      created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
      updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX IF NOT EXISTS idx_todos_completed ON todos(completed);
  `);
}

// Initialize on import
initializeDatabase();

export interface Todo {
  id: number;
  title: string;
  completed: number;
  created_at: string;
  updated_at: string;
}

export interface CreateTodoInput {
  title: string;
}

export interface UpdateTodoInput {
  title?: string;
  completed?: boolean;
}

// Get all todos
export function getAllTodos(): Todo[] {
  const stmt = db.prepare("SELECT * FROM todos ORDER BY created_at DESC");
  return stmt.all() as Todo[];
}

// Get todo by ID
export function getTodoById(id: number): Todo | undefined {
  const stmt = db.prepare("SELECT * FROM todos WHERE id = ?");
  return stmt.get(id) as Todo | undefined;
}

// Create todo
export function createTodo(input: CreateTodoInput): Todo {
  const stmt = db.prepare(
    "INSERT INTO todos (title) VALUES (?) RETURNING *"
  );
  return stmt.get(input.title) as Todo;
}

// Update todo
export function updateTodo(id: number, input: UpdateTodoInput): Todo | undefined {
  const fields: string[] = [];
  const values: any[] = [];

  if (input.title !== undefined) {
    fields.push("title = ?");
    values.push(input.title);
  }

  if (input.completed !== undefined) {
    fields.push("completed = ?");
    values.push(input.completed ? 1 : 0);
  }

  if (fields.length === 0) {
    return getTodoById(id);
  }

  fields.push("updated_at = CURRENT_TIMESTAMP");
  values.push(id);

  const stmt = db.prepare(
    `UPDATE todos SET ${fields.join(", ")} WHERE id = ? RETURNING *`
  );
  return stmt.get(...values) as Todo | undefined;
}

// Delete todo
export function deleteTodo(id: number): boolean {
  const stmt = db.prepare("DELETE FROM todos WHERE id = ?");
  const result = stmt.run(id);
  return result.changes > 0;
}

// Delete all todos
export function deleteAllTodos(): number {
  const stmt = db.prepare("DELETE FROM todos");
  const result = stmt.run();
  return result.changes;
}

export default db;
