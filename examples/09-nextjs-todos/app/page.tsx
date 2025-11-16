"use client";

import { useState, useEffect } from "react";

interface Todo {
  id: number;
  title: string;
  completed: number;
  created_at: string;
  updated_at: string;
}

export default function Home() {
  const [todos, setTodos] = useState<Todo[]>([]);
  const [newTodo, setNewTodo] = useState("");
  const [loading, setLoading] = useState(true);

  // Fetch todos
  const fetchTodos = async () => {
    try {
      const res = await fetch("/api/todos");
      const data = await res.json();
      setTodos(data);
    } catch (error) {
      console.error("Failed to fetch todos:", error);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchTodos();
  }, []);

  // Add todo
  const addTodo = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newTodo.trim()) return;

    try {
      const res = await fetch("/api/todos", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title: newTodo }),
      });
      const todo = await res.json();
      setTodos([todo, ...todos]);
      setNewTodo("");
    } catch (error) {
      console.error("Failed to add todo:", error);
    }
  };

  // Toggle todo
  const toggleTodo = async (id: number, completed: boolean) => {
    try {
      const res = await fetch(`/api/todos/${id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ completed: !completed }),
      });
      const updatedTodo = await res.json();
      setTodos(todos.map((t) => (t.id === id ? updatedTodo : t)));
    } catch (error) {
      console.error("Failed to toggle todo:", error);
    }
  };

  // Delete todo
  const deleteTodo = async (id: number) => {
    try {
      await fetch(`/api/todos/${id}`, { method: "DELETE" });
      setTodos(todos.filter((t) => t.id !== id));
    } catch (error) {
      console.error("Failed to delete todo:", error);
    }
  };

  if (loading) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-zinc-50 dark:bg-zinc-900">
        <div className="text-zinc-600 dark:text-zinc-400">Loading...</div>
      </div>
    );
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-zinc-50 dark:bg-zinc-900 p-4">
      <div className="w-full max-w-2xl">
        <div className="bg-white dark:bg-zinc-800 rounded-lg shadow-lg p-8">
          <h1 className="text-3xl font-bold text-zinc-900 dark:text-zinc-100 mb-8">
            Todo App
          </h1>

          {/* Add todo form */}
          <form onSubmit={addTodo} className="mb-8">
            <div className="flex gap-2">
              <input
                type="text"
                value={newTodo}
                onChange={(e) => setNewTodo(e.target.value)}
                placeholder="What needs to be done?"
                className="flex-1 px-4 py-3 rounded-lg border border-zinc-300 dark:border-zinc-600 bg-white dark:bg-zinc-700 text-zinc-900 dark:text-zinc-100 placeholder-zinc-400 focus:outline-none focus:ring-2 focus:ring-blue-500"
              />
              <button
                type="submit"
                className="px-6 py-3 bg-blue-500 hover:bg-blue-600 text-white rounded-lg font-medium transition-colors"
              >
                Add
              </button>
            </div>
          </form>

          {/* Todo list */}
          <div className="space-y-2">
            {todos.length === 0 ? (
              <p className="text-center text-zinc-500 dark:text-zinc-400 py-8">
                No todos yet. Add one above!
              </p>
            ) : (
              todos.map((todo) => (
                <div
                  key={todo.id}
                  className="flex items-center gap-3 p-4 bg-zinc-50 dark:bg-zinc-700 rounded-lg hover:bg-zinc-100 dark:hover:bg-zinc-600 transition-colors"
                >
                  <input
                    type="checkbox"
                    checked={todo.completed === 1}
                    onChange={() => toggleTodo(todo.id, todo.completed === 1)}
                    className="w-5 h-5 rounded border-zinc-300 dark:border-zinc-500 text-blue-500 focus:ring-blue-500 focus:ring-offset-0 cursor-pointer"
                  />
                  <span
                    className={`flex-1 text-zinc-900 dark:text-zinc-100 ${
                      todo.completed === 1
                        ? "line-through text-zinc-500 dark:text-zinc-400"
                        : ""
                    }`}
                  >
                    {todo.title}
                  </span>
                  <button
                    onClick={() => deleteTodo(todo.id)}
                    className="px-3 py-1 text-sm text-red-500 hover:text-red-600 hover:bg-red-50 dark:hover:bg-red-900/20 rounded transition-colors"
                  >
                    Delete
                  </button>
                </div>
              ))
            )}
          </div>

          {/* Stats */}
          {todos.length > 0 && (
            <div className="mt-6 pt-6 border-t border-zinc-200 dark:border-zinc-600 text-sm text-zinc-600 dark:text-zinc-400">
              {todos.filter((t) => t.completed === 0).length} item(s) left •{" "}
              {todos.filter((t) => t.completed === 1).length} completed
            </div>
          )}
        </div>

        <div className="mt-8 text-center text-sm text-zinc-500 dark:text-zinc-400">
          Built with Next.js • Deployed with Tako CLI
        </div>
      </div>
    </div>
  );
}
